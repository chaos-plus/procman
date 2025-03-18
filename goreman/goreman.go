package goreman

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
)

// version is the git tag at the time of build and is used to denote the
// binary's current version. This value is supplied as an ldflag at compile
// time by goreleaser (see .goreleaser.yml).
const (
	name     = "goreman"
	version  = "0.3.16"
	revision = "HEAD"
)

func Usage() {
	fmt.Fprint(os.Stderr, `Tasks:
  goreman check                      # Show entries in Procfile
  goreman help [TASK]                # Show this help
  goreman export [FORMAT] [LOCATION] # Export the apps to another process
                                       (upstart)
  goreman run COMMAND [PROCESS...]   # Run a command
                                       start
                                       stop
                                       stop-all
                                       restart
                                       restart-all
                                       list
                                       status
  goreman start [PROCESS]            # Start the application
  goreman version                    # Display Goreman version

Options:
`)
	flag.PrintDefaults()
	os.Exit(0)
}

func ShowVersion() {
	fmt.Fprintf(os.Stdout, "%s\n", version)
	os.Exit(0)
}

// -- process information structure.
type ProcInfo struct {
	name       string
	cmdline    string
	cmd        *exec.Cmd
	port       uint
	setPort    bool
	colorIndex int

	// True if we called stopProc to kill the process, in which case an
	// *os.ExitError is not the fault of the subprocess
	stoppedBySupervisor bool

	mu      sync.Mutex
	cond    *sync.Cond
	waitErr error
	logTime bool
}

var mu sync.Mutex

// process informations named with proc.
var procs []*ProcInfo

var maxProcNameLength = 0

var re = regexp.MustCompile(`\$([a-zA-Z]+[a-zA-Z0-9_]+)`)

type Config struct {
	Args []string

	Procfile       string `yaml:"procfile" mapstructure:"procfile" description:"proc file" default:"Procfile"`
	StartRpcServer bool   `yaml:"rpc-server" mapstructure:"rpc-server" description:"start an RPC server" default:"true"`
	RpcPort        uint   `yaml:"port" mapstructure:"port" description:"port" default:"8555"`
	BaseDir        string `yaml:"basedir" mapstructure:"basedir" description:"base directory" default:""`
	BasePort       uint   `yaml:"baseport" mapstructure:"baseport" description:"base port" default:"5000"`
	ExitOnError    bool   `yaml:"exit_on_error" mapstructure:"exit_on_error" description:"exit on error" default:"false"`
	ExitOnStop     bool   `yaml:"exit_on_stop" mapstructure:"exit_on_stop" description:"exit on stop" default:"true"`
	SetPorts       bool   `yaml:"set_ports" mapstructure:"set_ports" description:"False to avoid setting PORT env var for each subprocess" default:"true"`
	LogTime        bool   `yaml:"logtime" mapstructure:"logtime" description:"show timestamp in log" default:"true"`
}

// read Procfile and parse it.
func readProcfile(cfg *Config) error {
	if _, err := os.Stat(cfg.Procfile); err != nil {
		if os.IsNotExist(err) {
			return errors.New("procfile does not exist:" + cfg.Procfile)
		}
		return err
	}
	content, err := os.ReadFile(cfg.Procfile)
	if err != nil {
		return err
	}
	mu.Lock()
	defer mu.Unlock()

	procs = []*ProcInfo{}
	index := 0
	for _, line := range strings.Split(string(content), "\n") {
		tokens := strings.SplitN(line, ":", 2)
		if len(tokens) != 2 || tokens[0][0] == '#' {
			continue
		}
		k, v := strings.TrimSpace(tokens[0]), strings.TrimSpace(tokens[1])
		if runtime.GOOS == "windows" {
			v = re.ReplaceAllStringFunc(v, func(s string) string {
				return "%" + s[1:] + "%"
			})
		}
		proc := &ProcInfo{name: k, cmdline: v, colorIndex: index, logTime: cfg.LogTime}
		if cfg.SetPorts {
			proc.setPort = true
			proc.port = cfg.BasePort
			cfg.BasePort += 100
		}
		proc.cond = sync.NewCond(&proc.mu)
		procs = append(procs, proc)
		if len(k) > maxProcNameLength {
			maxProcNameLength = len(k)
		}
		index = (index + 1) % len(colors)
	}
	if len(procs) == 0 {
		return errors.New("no valid entry")
	}
	return nil
}

func defaultServer(serverPort uint) string {
	if s, ok := os.LookupEnv("GOREMAN_RPC_SERVER"); ok {
		return s
	}
	return fmt.Sprintf("127.0.0.1:%d", defaultPort())
}

func defaultAddr() string {
	if s, ok := os.LookupEnv("GOREMAN_RPC_ADDR"); ok {
		return s
	}
	return "0.0.0.0"
}

// default port
func defaultPort() uint {
	s := os.Getenv("GOREMAN_RPC_PORT")
	if s != "" {
		i, err := strconv.Atoi(s)
		if err == nil {
			return uint(i)
		}
	}
	return 8555
}

// command: check. show Procfile entries.
func check(cfg *Config) error {
	err := readProcfile(cfg)
	if err != nil {
		return err
	}

	mu.Lock()
	defer mu.Unlock()

	keys := make([]string, len(procs))
	i := 0
	for _, proc := range procs {
		keys[i] = proc.name
		i++
	}
	sort.Strings(keys)
	fmt.Printf("valid procfile detected (%s)\n", strings.Join(keys, ", "))
	return nil
}

func findProc(name string) *ProcInfo {
	mu.Lock()
	defer mu.Unlock()

	for _, proc := range procs {
		if proc.name == name {
			return proc
		}
	}
	return nil
}

// command: start. spawn procs.
func start(ctx context.Context, sig <-chan os.Signal, cfg *Config) error {
	err := readProcfile(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	// Cancel the RPC server when procs have returned/errored, cancel the
	// context anyway in case of early return.
	defer cancel()
	if len(cfg.Args) > 1 {
		tmp := make([]*ProcInfo, 0, len(cfg.Args[1:]))
		maxProcNameLength = 0
		for _, v := range cfg.Args[1:] {
			proc := findProc(v)
			if proc == nil {
				return errors.New("unknown proc: " + v)
			}
			tmp = append(tmp, proc)
			if len(v) > maxProcNameLength {
				maxProcNameLength = len(v)
			}
		}
		mu.Lock()
		procs = tmp
		mu.Unlock()
	}
	godotenv.Load()
	rpcChan := make(chan *rpcMessage, 10)
	if cfg.StartRpcServer {
		go startServer(ctx, rpcChan, cfg.RpcPort)
	}
	procsErr := startProcs(ctx, sig, rpcChan, cfg)
	return procsErr
}

func ParseConfig(args []string) (*Config, error) {
	fs := flag.NewFlagSet("goreman", flag.ExitOnError)
	return ParseConfigWithFlagSet(fs, args)
}

func ParseConfigWithFlagSet(fs *flag.FlagSet, args []string) (*Config, error) {
	cfg := &Config{}
	fs.StringVar(&cfg.Procfile, "f", "Procfile", "proc file")
	fs.UintVar(&cfg.RpcPort, "p", defaultPort(), "port")
	fs.BoolVar(&cfg.StartRpcServer, "rpc-server", true, "Start an RPC server listening on "+defaultAddr())
	fs.StringVar(&cfg.BaseDir, "basedir", "", "base directory")
	fs.UintVar(&cfg.BasePort, "b", defaultPort(), "base number of port")
	fs.BoolVar(&cfg.SetPorts, "set-ports", true, "False to avoid setting PORT env var for each subprocess")
	fs.BoolVar(&cfg.ExitOnError, "exit-on-error", false, "Exit goreman if a subprocess quits with a nonzero return code")
	fs.BoolVar(&cfg.ExitOnStop, "exit-on-stop", true, "Exit goreman if all subprocesses stop")
	fs.BoolVar(&cfg.LogTime, "logtime", true, "show timestamp in log")
	fs.Parse(args)
	if len(fs.Args()) > 0 {
		cfg.Args = fs.Args()
	} else {
		cfg.Args = args
	}
	return cfg, nil
}

func Main() {
	MainWithConfig(nil)
}

func MainWithConfig(cfg *Config) {
	var err error

	if cfg == nil {
		cfg = &Config{}
	}

	if cfg.BaseDir != "" {
		err = os.Chdir(cfg.BaseDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "goreman: %s\n", err.Error())
			os.Exit(1)
		}
	}
	cmd := ""
	if len(cfg.Args) > 0 {
		cmd = cfg.Args[0]
	} else if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "check":
		err = check(cfg)
	case "help":
		Usage()
	case "run":
		if len(cfg.Args) >= 2 {
			cmd, args := cfg.Args[1], cfg.Args[2:]
			err = run(cmd, args, cfg.RpcPort)
		} else {
			Usage()
		}
	case "export":
		if len(cfg.Args) == 3 {
			format, path := cfg.Args[1], cfg.Args[2]
			err = export(cfg, format, path)
		} else {
			Usage()
		}
	case "start":
		c := notifyCh()
		err = start(context.Background(), c, cfg)
	case "version":
		ShowVersion()
	default:
		Usage()
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[0], err.Error())
		os.Exit(1)
	}
}
