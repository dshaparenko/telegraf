package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/fatih/color"
	"github.com/influxdata/tail/watch"
	"gopkg.in/fsnotify.v1"
	"gopkg.in/tomb.v1"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/agent"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/logger"
	"github.com/influxdata/telegraf/plugins/aggregators"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/parsers"
	"github.com/influxdata/telegraf/plugins/processors"
	"github.com/influxdata/telegraf/plugins/secretstores"
)

var stop chan struct{}

type GlobalFlags struct {
	config                 []string
	configDir              []string
	testWait               int
	configURLRetryAttempts int
	configURLWatchInterval time.Duration
	watchConfig            string
	pidFile                string
	plugindDir             string
	password               string
	oldEnvBehavior         bool
	test                   bool
	debug                  bool
	once                   bool
	quiet                  bool
	unprotected            bool
}

type WindowFlags struct {
	service             string
	serviceName         string
	serviceDisplayName  string
	serviceRestartDelay string
	serviceAutoRestart  bool
	console             bool
}

type App interface {
	Init(<-chan error, Filters, GlobalFlags, WindowFlags)
	Run() error

	// Secret store commands
	ListSecretStores() ([]string, error)
	GetSecretStore(string) (telegraf.SecretStore, error)
}

type Telegraf struct {
	pprofErr <-chan error

	inputFilters       []string
	outputFilters      []string
	configFiles        []string
	secretstoreFilters []string

	GlobalFlags
	WindowFlags
}

func (t *Telegraf) Init(pprofErr <-chan error, f Filters, g GlobalFlags, w WindowFlags) {
	t.pprofErr = pprofErr
	t.inputFilters = f.input
	t.outputFilters = f.output
	t.secretstoreFilters = f.secretstore
	t.GlobalFlags = g
	t.WindowFlags = w

	// Disable secret protection before performing any other operation
	if g.unprotected {
		log.Println("W! Running without secret protection!")
		config.DisableSecretProtection()
	}

	// Set global password
	if g.password != "" {
		config.Password = config.NewSecret([]byte(g.password))
	}

	// Set environment replacement behavior
	config.OldEnvVarReplacement = g.oldEnvBehavior
}

func (t *Telegraf) ListSecretStores() ([]string, error) {
	c, err := t.loadConfiguration()
	if err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(c.SecretStores))
	for k := range c.SecretStores {
		ids = append(ids, k)
	}
	return ids, nil
}

func (t *Telegraf) GetSecretStore(id string) (telegraf.SecretStore, error) {
	t.quiet = true
	c, err := t.loadConfiguration()
	if err != nil {
		return nil, err
	}

	store, found := c.SecretStores[id]
	if !found {
		return nil, errors.New("unknown secret store")
	}

	return store, nil
}

func (t *Telegraf) reloadLoop() error {
	reloadConfig := false
	cfg, err := t.loadConfiguration()
	if err != nil {
		return err
	}

	reload := make(chan bool, 1)
	reload <- true
	for <-reload {
		reload <- false
		ctx, cancel := context.WithCancel(context.Background())

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGHUP,
			syscall.SIGTERM, syscall.SIGINT)

		if t.watchConfig != "" {
			for _, fConfig := range t.configFiles {
				if isURL(fConfig) {
					continue
				}

				if _, err := os.Stat(fConfig); err != nil {
					log.Printf("W! Cannot watch config %s: %s", fConfig, err)
				} else {
					go t.watchLocalConfig(signals, fConfig)
				}
			}
			// tsv: watch config dirs
			for _, dir := range t.configDir {
				if _, err := os.Stat(dir); err == nil {
					go t.watchLocalConfigDir(signals, dir, "\\.watchman-cookie.*")
				} else {
					log.Printf("W! Cannot watch config dir %s: %s", dir, err)
				}
			}
		}

		if t.configURLWatchInterval > 0 {
			remoteConfigs := make([]string, 0)
			for _, fConfig := range t.configFiles {
				if isURL(fConfig) {
					remoteConfigs = append(remoteConfigs, fConfig)
				}
			}
			if len(remoteConfigs) > 0 {
				go t.watchRemoteConfigs(signals, t.configURLWatchInterval, remoteConfigs)
			}
		}

		go func() {
			select {
			case sig := <-signals:
				if sig == syscall.SIGHUP {
					log.Println("I! Reloading Telegraf config")
					<-reload
					reload <- true
				}
				cancel()
			case err := <-t.pprofErr:
				log.Printf("E! pprof server failed: %v", err)
				cancel()
			case <-stop:
				cancel()
			}
		}()

		err := t.runAgent(ctx, cfg, reloadConfig)
		if err != nil && !errors.Is(err, context.Canceled) {
			return fmt.Errorf("[telegraf] Error running agent: %w", err)
		}
		reloadConfig = true
	}

	return nil
}

// tsv: watch config directory for new files
func (t *Telegraf) watchLocalConfigDir(signals chan os.Signal, dir, exclusion string) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("E! Error watching config dir: %s\n", err)
		return
	}
	defer watcher.Close()

	re := regexp.MustCompile(exclusion)

	done := make(chan bool)
	go func() {
		for {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					continue
				}
				log.Printf("I! Event watching config dir: %s\n", event)

				if re.MatchString(event.Name) {
					continue
				}

				if event.Op&fsnotify.Remove == fsnotify.Remove {
					signals <- syscall.SIGHUP
					return
				}

				if event.Op&fsnotify.Create == fsnotify.Create {
					signals <- syscall.SIGHUP
					return
				}

			case err, ok := <-watcher.Errors:
				if !ok {
					continue
				}
				log.Printf("E! Error watching config dir: %s\n", err)
			}
		}
	}()

	err = filepath.Walk(dir, func(path string, f os.FileInfo, err error) error {

		if f.IsDir() {
			err := watcher.Add(path)
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Printf("E! Error watching config subdir: %s\n", err)
	}
	<-done
}

func (t *Telegraf) watchLocalConfig(signals chan os.Signal, fConfig string) {
	var mytomb tomb.Tomb
	var watcher watch.FileWatcher
	if t.watchConfig == "poll" {
		watcher = watch.NewPollingFileWatcher(fConfig)
	} else {
		watcher = watch.NewInotifyFileWatcher(fConfig)
	}
	changes, err := watcher.ChangeEvents(&mytomb, 0)
	if err != nil {
		log.Printf("E! Error watching config: %s\n", err)
		return
	}
	log.Printf("I! Config watcher started for %s\n", fConfig)
	select {
	case <-changes.Modified:
		log.Println("I! Config file modified")
	case <-changes.Deleted:
		// deleted can mean moved. wait a bit a check existence
		<-time.After(time.Second)
		if _, err := os.Stat(fConfig); err == nil {
			log.Printf("I! Config file overwritten %s\n", fConfig)
		} else {
			log.Printf("W! Config file deleted %s\n", fConfig)
			// tsv: issue here if several renames happended
			/*if err := watcher.BlockUntilExists(&mytomb); err != nil {
				log.Printf("E! Cannot watch for config: %s\n", err.Error())
				return
			}
			log.Println("I! Config file appeared")*/
		}
	case <-changes.Truncated:
		log.Println("I! Config file truncated")
	case <-mytomb.Dying():
		log.Println("I! Config watcher ended")
		return
	}
	mytomb.Done()
	signals <- syscall.SIGHUP
}

func (t *Telegraf) watchRemoteConfigs(signals chan os.Signal, interval time.Duration, remoteConfigs []string) {
	configs := strings.Join(remoteConfigs, ", ")
	log.Printf("I! Remote config watcher started for: %s\n", configs)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	lastModified := make(map[string]string, len(remoteConfigs))
	for {
		select {
		case <-signals:
			return
		case <-ticker.C:
			for _, configURL := range remoteConfigs {
				u, err := url.Parse(configURL)
				if err != nil {
					log.Printf("W! Error parsing config URL, %s: %s\n", configURL, err)
					continue
				}
				rawQuery, err := config.AddHostParams(u)
				if err != nil {
					log.Printf("W! Error adding params to config URL, %s: %s\n", configURL, err)
					continue
				}
				u.RawQuery = rawQuery
				URLparam := u.String()

				resp, err := http.Head(URLparam) //nolint: gosec // user provided URL
				if err != nil {
					log.Printf("W! Error fetching config URL, %s: %s\n", configURL, err)
					continue
				}
				resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					log.Printf("E! Failed to fetch HTTP config: %s", resp.Status)
					continue
				}

				modified := resp.Header.Get("Last-Modified")
				if modified == "" {
					log.Printf("E! Last-Modified header not found, stopping the watcher for %s\n", configURL)
					delete(lastModified, configURL)
				}

				if lastModified[configURL] == "" {
					lastModified[configURL] = modified
				} else if lastModified[configURL] != modified {
					log.Printf("I! Remote config modified: %s\n", configURL)
					signals <- syscall.SIGHUP
					return
				}
			}
		}
	}
}

func (t *Telegraf) loadConfiguration() (*config.Config, error) {
	// If no other options are specified, load the config file and run.
	c := config.NewConfig()
	c.Agent.Quiet = t.quiet
	c.OutputFilters = t.outputFilters
	c.InputFilters = t.inputFilters
	c.SecretStoreFilters = t.secretstoreFilters

	var configFiles []string

	configFiles = append(configFiles, t.config...)
	for _, fConfigDirectory := range t.configDir {
		files, err := config.WalkDirectory(fConfigDirectory)
		if err != nil {
			return c, err
		}
		configFiles = append(configFiles, files...)
	}

	// load default config paths if none are found
	if len(configFiles) == 0 {
		defaultFiles, err := config.GetDefaultConfigPath()
		if err != nil {
			return nil, fmt.Errorf("unable to load default config paths: %w", err)
		}
		configFiles = append(configFiles, defaultFiles...)
	}

	c.Agent.ConfigURLRetryAttempts = t.configURLRetryAttempts
	t.configFiles = configFiles
	if err := c.LoadAll(configFiles...); err != nil {
		return c, err
	}
	return c, nil
}

func (t *Telegraf) runAgent(ctx context.Context, c *config.Config, reloadConfig bool) error {
	var err error
	if reloadConfig {
		if c, err = t.loadConfiguration(); err != nil {
			return err
		}
	}

	if !(t.test || t.testWait != 0) && len(c.Outputs) == 0 {
		return errors.New("no outputs found, did you provide a valid config file?")
	}
	if t.plugindDir == "" && len(c.Inputs) == 0 {
		return errors.New("no inputs found, did you provide a valid config file?")
	}

	if int64(c.Agent.Interval) <= 0 {
		return fmt.Errorf("agent interval must be positive, found %v", c.Agent.Interval)
	}

	if int64(c.Agent.FlushInterval) <= 0 {
		return fmt.Errorf("agent flush_interval must be positive; found %v", c.Agent.Interval)
	}

	// Setup logging as configured.
	telegraf.Debug = c.Agent.Debug || t.debug
	logConfig := logger.LogConfig{
		Debug:               telegraf.Debug,
		Quiet:               c.Agent.Quiet || t.quiet,
		LogTarget:           c.Agent.LogTarget,
		Logfile:             c.Agent.Logfile,
		RotationInterval:    c.Agent.LogfileRotationInterval,
		RotationMaxSize:     c.Agent.LogfileRotationMaxSize,
		RotationMaxArchives: c.Agent.LogfileRotationMaxArchives,
		LogWithTimezone:     c.Agent.LogWithTimezone,
	}

	if err := logger.SetupLogging(logConfig); err != nil {
		return err
	}

	log.Printf("I! Starting Telegraf %s%s brought to you by InfluxData the makers of InfluxDB", internal.Version, internal.Customized)
	log.Printf("I! Available plugins: %d inputs, %d aggregators, %d processors, %d parsers, %d outputs, %d secret-stores",
		len(inputs.Inputs),
		len(aggregators.Aggregators),
		len(processors.Processors),
		len(parsers.Parsers),
		len(outputs.Outputs),
		len(secretstores.SecretStores),
	)
	log.Printf("I! Loaded inputs: %s", strings.Join(c.InputNames(), " "))
	log.Printf("I! Loaded aggregators: %s", strings.Join(c.AggregatorNames(), " "))
	log.Printf("I! Loaded processors: %s", strings.Join(c.ProcessorNames(), " "))
	log.Printf("I! Loaded secretstores: %s", strings.Join(c.SecretstoreNames(), " "))
	if !t.once && (t.test || t.testWait != 0) {
		log.Print("W! " + color.RedString("Outputs are not used in testing mode!"))
	} else {
		log.Printf("I! Loaded outputs: %s", strings.Join(c.OutputNames(), " "))
	}
	log.Printf("I! Tags enabled: %s", c.ListTags())

	if count, found := c.Deprecations["inputs"]; found && (count[0] > 0 || count[1] > 0) {
		log.Printf("W! Deprecated inputs: %d and %d options", count[0], count[1])
	}
	if count, found := c.Deprecations["aggregators"]; found && (count[0] > 0 || count[1] > 0) {
		log.Printf("W! Deprecated aggregators: %d and %d options", count[0], count[1])
	}
	if count, found := c.Deprecations["processors"]; found && (count[0] > 0 || count[1] > 0) {
		log.Printf("W! Deprecated processors: %d and %d options", count[0], count[1])
	}
	if count, found := c.Deprecations["outputs"]; found && (count[0] > 0 || count[1] > 0) {
		log.Printf("W! Deprecated outputs: %d and %d options", count[0], count[1])
	}
	if count, found := c.Deprecations["secretstores"]; found && (count[0] > 0 || count[1] > 0) {
		log.Printf("W! Deprecated secretstores: %d and %d options", count[0], count[1])
	}

	// Compute the amount of locked memory needed for the secrets
	if !t.GlobalFlags.unprotected {
		required := 3 * c.NumberSecrets * uint64(os.Getpagesize())
		available := getLockedMemoryLimit()
		if required > available {
			required /= 1024
			available /= 1024
			log.Printf("I! Found %d secrets...", c.NumberSecrets)
			msg := fmt.Sprintf("Insufficient lockable memory %dkb when %dkb is required.", available, required)
			msg += " Please increase the limit for Telegraf in your Operating System!"
			log.Printf("W! " + color.RedString(msg))
		}
	}
	ag := agent.NewAgent(c)

	// Notify systemd that telegraf is ready
	// SdNotify() only tries to notify if the NOTIFY_SOCKET environment is set, so it's safe to call when systemd isn't present.
	// Ignore the return values here because they're not valid for platforms that don't use systemd.
	// For platforms that use systemd, telegraf doesn't log if the notification failed.
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)

	if t.once {
		wait := time.Duration(t.testWait) * time.Second
		return ag.Once(ctx, wait)
	}

	if t.test || t.testWait != 0 {
		wait := time.Duration(t.testWait) * time.Second
		return ag.Test(ctx, wait)
	}

	if t.pidFile != "" {
		f, err := os.OpenFile(t.pidFile, os.O_CREATE|os.O_WRONLY, 0640)
		if err != nil {
			log.Printf("E! Unable to create pidfile: %s", err)
		} else {
			fmt.Fprintf(f, "%d\n", os.Getpid())

			err = f.Close()
			if err != nil {
				return err
			}

			defer func() {
				err := os.Remove(t.pidFile)
				if err != nil {
					log.Printf("E! Unable to remove pidfile: %s", err)
				}
			}()
		}
	}

	return ag.Run(ctx)
}

// isURL checks if string is valid url
func isURL(str string) bool {
	u, err := url.Parse(str)
	return err == nil && u.Scheme != "" && u.Host != ""
}
