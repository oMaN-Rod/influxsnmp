package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kardianos/osext"
	snmp "github.com/paulstuart/snmputil"
	"gopkg.in/gcfg.v1"
)

const layout = "2006-01-02 15:04:05"

type SnmpConfig struct {
	Host      string `gcfg:"host"`
	Community string `gcfg:"community"`
	Version   string `gcfg:"version"`
	Port      int    `gcfg:"port"`
	Retries   int    `gcfg:"retries"`
	Timeout   int    `gcfg:"timeout"`
	Freq      int    `gcfg:"freq"`
	PortFile  string `gcfg:"portfile"`
	Config    string `gcfg:"config"`
	Mibs      string `gcfg:"mibs"`
}

type HTTPConfig struct {
	Port int `gcfg:"port"`
}

type GeneralConfig struct {
	LogDir  string `gcfg:"logdir"`
	OidFile string `gcfg:"oidfile"`
}

type MibConfig struct {
	Scalers bool     `gcfg:"scalers"`
	Name    string   `gcfg:"name"`
	Columns []string `gcfg:"column"`
	Regexps []string `gcfg:"regexp"`
	Keep    bool     `gcfg:"keep"`
}

type SystemStatus struct {
	Period, Started string
	Uptime          string
	DB, LogFile     string
	DebugAction     string
	Influx          map[string]*InfluxConfig
	SNMP            map[string]*SnmpConfig
	SnmpStats       map[string]snmp.SnmpStats
}

var (
	quit          = make(chan struct{})
	verbose       bool
	startTime     = time.Now()
	testing       bool
	snmpNames     bool
	httpPort      = 8080
	appdir, _     = osext.ExecutableFolder()
	logDir        = filepath.Join(appdir, "log")
	oidFile       = filepath.Join(appdir, "oids.txt")
	configFile    = filepath.Join(appdir, "config.gcfg")
	errorLog      *os.File
	errorDuration = time.Duration(10 * time.Minute)
	errorPeriod   = errorDuration.String()
	errorMax      = 100
	errorName     string
	senders       = map[string]Sender{}
	batchSize     = 1024
	queueSize     = 65535
	period        = 10
	snmpStats     = map[string]chan snmp.StatsChan{}

	cfg = struct {
		Snmp    map[string]*SnmpConfig
		Mibs    map[string]*MibConfig
		Influx  map[string]*InfluxConfig
		HTTP    HTTPConfig
		General GeneralConfig
	}{}
)

func status() SystemStatus {
	return SystemStatus{
		LogFile:   errorName,
		Started:   startTime.Format(layout),
		Uptime:    time.Now().Sub(startTime).String(),
		Period:    errorPeriod,
		Influx:    cfg.Influx,
		SNMP:      cfg.Snmp,
		SnmpStats: Stats(),
	}
}

func Stats() map[string]snmp.SnmpStats {
	stats := map[string]snmp.SnmpStats{}
	for name, c := range snmpStats {
		ret := make(chan snmp.SnmpStats)
		c <- ret
		stats[name] = <-ret
	}
	return stats
}

func flags() *flag.FlagSet {
	var f flag.FlagSet
	f.BoolVar(&testing, "testing", testing, "print data w/o saving")
	f.BoolVar(&snmpNames, "names", snmpNames, "print column names and exit")
	f.StringVar(&configFile, "config", configFile, "config file")
	f.BoolVar(&verbose, "verbose", verbose, "verbose mode")
	f.IntVar(&httpPort, "http", httpPort, "http port")
	f.StringVar(&logDir, "logs", logDir, "log directory")
	f.StringVar(&oidFile, "oids", oidFile, "OIDs file")
	f.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		f.VisitAll(func(flag *flag.Flag) {
			format := "%10s: %s\n"
			fmt.Fprintf(os.Stderr, format, "-"+flag.Name, flag.Usage)
		})
		fmt.Fprintf(os.Stderr, "\nAll settings can be set in config file: %s\n", configFile)
		os.Exit(1)

	}
	return &f
}

func init() {
	log.SetOutput(os.Stderr)

	// parse first time to see if config file is being specified
	f := flags()
	f.Parse(os.Args[1:])

	// now load up config settings
	if _, err := os.Stat(configFile); err != nil {
		log.Fatal(err)
	}
	data, err := ioutil.ReadFile(configFile)
	if err != nil {
		log.Fatal(err)
	}
	err = gcfg.ReadStringInto(&cfg, string(data))
	if err != nil {
		log.Fatalf("Failed to parse gcfg data: %s", err)
	}
	httpPort = cfg.HTTP.Port

	if len(cfg.General.LogDir) > 0 {
		logDir = cfg.General.LogDir
	}
	if len(cfg.General.OidFile) > 0 {
		oidFile = cfg.General.OidFile
	}

	// only run when one needs to see the interface names of the device
	/*
		if snmpNames {
			for _, c := range cfg.Snmp {
				fmt.Println("\nSNMP host:", c.Host)
				fmt.Println("=========================================")
				printSnmpNames(c)
			}
			os.Exit(0)
		}
	*/

	// re-read cmd line args to override as indicated
	f = flags()
	f.Parse(os.Args[1:])
	os.Mkdir(logDir, 0755)

	for name, c := range cfg.Influx {
		c.Batch.Database = c.DB
		sender, err := c.NewSender(batchSize, queueSize, period)
		if err != nil {
			panic(err)
		}
		senders[name] = sender
	}
	if err := snmp.LoadOIDFile(oidFile); err != nil {
		panic(err)
	}
	var ferr error
	errorName = fmt.Sprintf("error.%d.log", cfg.HTTP.Port)
	errorPath := filepath.Join(logDir, errorName)
	errorLog, ferr = os.OpenFile(errorPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0664)
	if ferr != nil {
		log.Fatal("Can't open error log:", ferr)
	}
}

func errFn(err error) {
	log.Println(err)
}

func gather(send Sender, c *SnmpConfig, mib *MibConfig) {
	crit := snmp.Criteria{
		OID:     mib.Name,
		Regexps: mib.Regexps,
		Keep:    mib.Keep,
	}
	name := fmt.Sprintf("%s/%s", c.Host, mib.Name)
	status := make(chan snmp.StatsChan)
	snmpStats[name] = status
	p := snmp.Profile{
		Host:      c.Host,
		Community: c.Community,
		Version:   c.Version,
		Port:      c.Port,
		Retries:   c.Retries,
		Timeout:   c.Timeout,
	}

	sender := func(name string, tags map[string]string, value interface{}, when time.Time) error {
		values := map[string]interface{}{"value": value}
		return send(name, tags, values, when)
	}
	if err := snmp.Bulkwalker(p, crit, sender, c.Freq, errFn, status); err != nil {
		panic(err)
	}
}

func main() {
	snmp.Verbose = verbose
	for name, c := range cfg.Snmp {
		send, ok := senders[name]
		if !ok {
			send, ok = senders["*"]
			if !ok {
				panic("No sender for: " + name)
			}
		}
		if len(c.Mibs) > 0 {
			for _, m := range strings.Fields(c.Mibs) {
				mib, ok := cfg.Mibs[m]
				if !ok {
					panic("no mib found for: " + m)
				}
				gather(send, c, mib)
			}
			continue
		}
		mib, ok := cfg.Mibs[name]
		if !ok {
			if mib, ok = cfg.Mibs["*"]; !ok {
				panic("no mib found for: " + name)
			}
		}
		gather(send, c, mib)
	}
	if httpPort > 0 {
		webServer(httpPort)
	} else {
		<-quit
	}
	errorLog.Close()
}
