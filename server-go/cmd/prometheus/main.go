// RamiCore Go host: HTTP/WS, agents, tools and the desktop sidecars.
//
//	agents  : detect Claude Code / Codex and check npm for newer versions
//	prompt  : run one prompt through Go relay -> Claude Agent SDK sidecar
//	serve   : Prometheus wire protocol v1 on /ws + the existing web UI
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"prometheus-go/internal/agents"
	"prometheus-go/internal/config"
	"prometheus-go/internal/host"
	"prometheus-go/internal/mcpconfig"
	"prometheus-go/internal/relay"
	"prometheus-go/internal/server"
	"prometheus-go/internal/sidecar"
)

type options struct {
	Model      string
	Cwd        string
	Web        string
	Node       string
	Sidecar    string
	PluginHost string
	Token      string
	Native     bool
	AllowWrite bool
}

type setup struct {
	relay       *relay.Relay
	claude      agents.Agent
	codex       agents.Agent
	model       string
	gateway     string
	gatewayList []any
	env         map[string]string
	node        string
	script      string
	cwd         string
	webRoot     string
	allowWrite  bool
	piEntry     string
	piVersion   string
	acpEntry    string
	acpVersion  string
	ocPath      string
	ocVersion   string
	pluginHost  string
	token       string
}

// uvPythonDirs returns directories that contain a uv-managed python executable.
// They are not normally on PATH, which made `python` look unavailable even
// though `uv python list` showed an interpreter.
func uvPythonDirs() []string {
	var roots []string
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if d := os.Getenv("APPDATA"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
	} else {
		roots = append(roots, filepath.Join(home, ".local", "share", "uv", "python"))
		if d := os.Getenv("XDG_DATA_HOME"); d != "" {
			roots = append(roots, filepath.Join(d, "uv", "python"))
		}
		roots = append(roots, filepath.Join(home, ".local", "bin"))
	}
	seen := map[string]bool{}
	var out []string
	for _, root := range roots {
		matches, _ := filepath.Glob(filepath.Join(root, "*"))
		sort.Sort(sort.Reverse(sort.StringSlice(matches)))
		for _, dir := range matches {
			if seen[dir] {
				continue
			}
			for _, exe := range []string{"python.exe", "python3", "python"} {
				if fi, err := os.Stat(filepath.Join(dir, exe)); err == nil && !fi.IsDir() {
					seen[dir] = true
					out = append(out, dir)
					break
				}
			}
		}
	}
	return out
}

func (s *setup) NewSidecar(sessionID, cwd, mcpURL, mcpToken, staticContext string) (*sidecar.Client, error) {
	if cwd == "" {
		cwd = s.cwd
	}
	env := make(map[string]string, len(s.env)+5)
	for k, v := range s.env {
		env[k] = v
	}
	env["PROMETHEUS_CWD"] = cwd
	// A project virtualenv should win over the global Python: uv/venv layouts put
	// the interpreter in Scripts (Windows) or bin (macOS/Linux).
	var pythonPaths []string
	for _, sub := range []string{"Scripts", "bin"} {
		dir := filepath.Join(cwd, ".venv", sub)
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			env["VIRTUAL_ENV"] = filepath.Join(cwd, ".venv")
			pythonPaths = append(pythonPaths, dir)
			break
		}
	}
	// uv installs Python outside PATH (its Windows directory has python.exe, but
	// ~/.local/bin only has versioned shims). Add the managed interpreters too so
	// `python` works without a project venv.
	pythonPaths = append(pythonPaths, uvPythonDirs()...)
	if len(pythonPaths) > 0 {
		path := env["PATH"]
		if path == "" {
			path = os.Getenv("PATH")
		}
		env["PATH"] = strings.Join(pythonPaths, string(os.PathListSeparator)) + string(os.PathListSeparator) + path
	}
	if mcpURL != "" {
		env["PROMETHEUS_MCP_URL"] = mcpURL
		env["PROMETHEUS_MCP_TOKEN"] = mcpToken
	}
	if extra := mcpconfig.Load(filepath.Join(config.Home(), "go-poc")); len(extra) > 0 {
		if raw, err := json.Marshal(extra); err == nil {
			env["PROMETHEUS_MCP_SERVERS"] = string(raw)
		}
	}
	if staticContext != "" {
		env["PROMETHEUS_STATIC_CONTEXT"] = staticContext
	}
	return sidecar.Start(sidecar.StartOptions{
		Node:   s.node,
		Script: s.script,
		Dir:    cwd,
		Env:    env,
		OnStderr: func(line string) {
			log.Printf("[sidecar] %s", line)
		},
	})
}

func (s *setup) Close() {
	if s.relay != nil {
		_ = s.relay.Close()
	}
}

func main() {
	var (
		agentsOnly = flag.Bool("agents", false, "list Claude Code / Codex versions and update availability")
		offline    = flag.Bool("offline", false, "skip npm version checks")
		prompt     = flag.String("prompt", "", "run one prompt and print the streamed answer")
		serve      = flag.Bool("serve", false, "start the HTTP/WebSocket host")
		addr       = flag.String("addr", "127.0.0.1:17420", "listen address for -serve")
		token      = flag.String("token", os.Getenv("PROMETHEUS_TOKEN"), "access token (required for non-loopback addresses)")
		model      = flag.String("model", "", "model id (default: PROMETHEUS_MODEL or the gateway's deepseek model)")
		native     = flag.Bool("native", false, "use the Claude CLI's own login instead of a Prometheus gateway")
		cwd        = flag.String("cwd", "", "working directory for the agent (default: current directory)")
		sidecarArg = flag.String("sidecar", "node/agent-host/index.mjs", "path to the Node agent-host script")
		nodeArg    = flag.String("node", "", "node executable (default: found on PATH)")
		webArg     = flag.String("web", "", "web UI directory (default: apps/web/dist when present)")
		pluginHost = flag.String("plugin-host", os.Getenv("PROMETHEUS_PLUGIN_URL"), "TypeScript plugin host URL (tools merged into the MCP bus)")
		allowWrite = flag.Bool("allow-write", false, "give the sidecar Bash/Edit/Write tools (permission bridge still gates them)")
		bootstrap  = flag.Bool("bootstrap", false, "install PI and build the bundled TypeScript plugin host")
		timeout    = flag.Duration("timeout", 10*time.Minute, "prompt timeout")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *agentsOnly {
		printAgents(ctx, !*offline)
		return
	}
	if *bootstrap {
		if err := runBootstrap(ctx); err != nil {
			log.Fatalf("bootstrap: %v", err)
		}
		return
	}
	if *prompt == "" && !*serve {
		flag.Usage()
		os.Exit(2)
	}

	s, err := build(ctx, options{Model: *model, Cwd: *cwd, Web: *webArg, Node: *nodeArg, Sidecar: *sidecarArg, PluginHost: *pluginHost, Token: *token, Native: *native, AllowWrite: *allowWrite})
	if err != nil {
		log.Fatalf("setup: %v", err)
	}
	defer s.Close()

	if *prompt != "" {
		runCtx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		sc, err := s.NewSidecar("prompt", "", "", "", "")
		if err != nil {
			log.Fatalf("sidecar: %v", err)
		}
		defer sc.Close()
		printer := &server.Printer{}
		if err := sc.Prompt(runCtx, *prompt, "", printer.Print); err != nil {
			log.Fatalf("prompt: %v", err)
		}
		return
	}

	provider := "claude-code"
	if s.gateway != "native" {
		provider = s.gateway
	}
	if !strings.HasPrefix(*addr, "127.0.0.1") && !strings.HasPrefix(*addr, "localhost") && s.token == "" {
		log.Fatal("非回环地址必须设置 -token 或 PROMETHEUS_TOKEN")
	}
	pluginURL := s.pluginHost
	if pluginURL == "" {
		if url, stop, err := startPluginHost(s.node, s.cwd); err == nil {
			pluginURL = url
			defer stop()
			log.Printf("TypeScript plugin host: %s", pluginURL)
		} else if !os.IsNotExist(err) {
			log.Printf("plugin host: %v", err)
		}
	}
	h := host.New(host.Config{
		Cwd:          s.cwd,
		Model:        s.model,
		Provider:     provider,
		Gateway:      s.gateway,
		GatewayList:  s.gatewayList,
		Claude:       s.claude,
		Codex:        s.codex,
		PINode:       s.node,
		PIEntry:      s.piEntry,
		PIVersion:    s.piVersion,
		ACPNode:      s.node,
		ACPEntry:     s.acpEntry,
		ACPVersion:   s.acpVersion,
		OpenCodePath: s.ocPath,
		OpenCodeVer:  s.ocVersion,
		RelayURL:     relayURL(s.relay),
		RelayToken:   relayToken(s.relay),
		PluginURL:    pluginURL,
		Token:        s.token,
		AllowWrite:   s.allowWrite,
		Offline:      *offline,
		StartSidecar: s.NewSidecar,
	})
	defer h.CloseAll()

	api := &server.Server{
		Host:      h,
		WebRoot:   s.WebRoot(),
		PluginURL: pluginURL,
		Token:     s.token,
		Cwd:       s.cwd,
		Model:     s.model,
		Gateway:   s.gateway,
		Claude:    s.claude,
		Offline:   *offline,
	}
	h.SetNetworkApply(api.ApplyNetwork)
	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	h.SetLocalPort(actualPort)
	// The desktop shell waits for this exact line before handing the port to the UI.
	fmt.Printf("PROMETHEUS_READY %d\n", actualPort)
	_ = os.Stdout.Sync()
	go func() {
		log.Printf("RamiCore listening on http://%s  (model=%s, gateway=%s, claude=%s, web=%s)",
			ln.Addr().String(), s.model, s.gateway, s.claude.Version, api.WebRoot)
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func (s *setup) WebRoot() string {
	return s.webRoot
}

func build(ctx context.Context, o options) (*setup, error) {
	cwd := o.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	cwd, _ = filepath.Abs(cwd)

	claude, err := agents.Resolve("claude-code")
	if err != nil {
		return nil, fmt.Errorf("Claude Code not found (PATH or %s/agents)", config.Home())
	}
	if v, err := agents.Version(ctx, claude.Path); err == nil {
		claude.Version = v
	}

	node := o.Node
	if node == "" {
		// Like the original desktop: do not silently download a runtime during
		// the splash screen. Tell the UI to offer the Node install instead.
		node = existingNode()
		if node == "" {
			return nil, fmt.Errorf("SETUP_NEEDED:node")
		}
	}
	script, err := resolveScript(o.Sidecar, cwd)
	if err != nil {
		return nil, err
	}
	sdk := os.Getenv("CLAUDE_SDK_PATH")
	if sdk == "" {
		candidates := []string{
			filepath.Join(cwd, "node", "agent-host", "sdk.mjs"),
			filepath.Join(cwd, "..", "node", "agent-host", "sdk.mjs"),
			filepath.Join(config.Home(), "packages", "claude-agent-sdk", "node_modules",
				"@anthropic-ai", "claude-agent-sdk", "sdk.mjs"),
		}
		if exe, err := os.Executable(); err == nil {
			candidates = append([]string{filepath.Join(filepath.Dir(exe), "node", "agent-host", "sdk.mjs")}, candidates...)
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				sdk = c
				break
			}
		}
	}
	if sdk == "" {
		return nil, fmt.Errorf("Claude Agent SDK not found (set CLAUDE_SDK_PATH)")
	}
	if _, err := os.Stat(sdk); err != nil {
		return nil, fmt.Errorf("Claude Agent SDK not found at %s (set CLAUDE_SDK_PATH)", sdk)
	}

	env := map[string]string{
		"CLAUDE_SDK_PATH":   sdk,
		"CLAUDE_EXECUTABLE": claude.Path,
		"PROMETHEUS_CWD":    cwd,
		"PROMETHEUS_MODEL":  o.Model,
	}
	if o.AllowWrite {
		env["PROMETHEUS_ALLOW_WRITE"] = "1"
	}
	s := &setup{claude: claude, model: o.Model, gateway: "native", env: env, node: node, script: script, cwd: cwd, allowWrite: o.AllowWrite, pluginHost: o.PluginHost, token: o.Token}
	if codex, err := agents.Resolve("codex"); err == nil {
		if v, err := agents.Version(ctx, codex.Path); err == nil {
			codex.Version = v
			s.codex = codex
		}
	}
	if entry, version := findPI(); entry != "" {
		s.piEntry = entry
		s.piVersion = version
	}
	if entry, version := findACP(); entry != "" {
		s.acpEntry = entry
		s.acpVersion = version
	}
	if bin, version := findOpenCode(); bin != "" {
		s.ocPath = bin
		s.ocVersion = version
	}

	if !o.Native {
		// A gateway is optional: a fresh install/CLI login can run Claude Code
		// natively, and the user can add a gateway later from Settings.
		if cfg, err := config.Load(); err != nil {
			log.Printf("no gateway config (%v); using the Claude Code login", err)
			s.gateway = "native"
			if s.model == "" {
				s.model = "sonnet"
			}
			env["PROMETHEUS_GATEWAY"] = "0"
			env["PROMETHEUS_MODEL"] = s.model
		} else if gw, base, err := cfg.PickAnthropic(); err != nil {
			log.Printf("%v; using the Claude Code login", err)
			s.gateway = "native"
			if s.model == "" {
				s.model = "sonnet"
			}
			env["PROMETHEUS_GATEWAY"] = "0"
			env["PROMETHEUS_MODEL"] = s.model
		} else {
			r, err := relay.Start(gw, base)
			if err != nil {
				return nil, err
			}
			s.relay = r
			s.gateway = gw.ID
			if s.model == "" {
				s.model = cfg.PickModel(gw)
			}
			s.gatewayList = config.Masked([]config.Gateway{*gw})
			env["PROMETHEUS_GATEWAY"] = "1"
			env["PROMETHEUS_MODEL"] = s.model
			env["ANTHROPIC_BASE_URL"] = r.URL
			env["ANTHROPIC_AUTH_TOKEN"] = r.Token
			env["ANTHROPIC_MODEL"] = s.model
			env["ANTHROPIC_SMALL_FAST_MODEL"] = s.model
			env["ANTHROPIC_API_KEY"] = "" // delete: the relay injects the real key
		}
	} else {
		env["PROMETHEUS_GATEWAY"] = "0"
		if s.model == "" {
			s.model = "sonnet"
		}
		env["PROMETHEUS_MODEL"] = s.model
	}

	if o.Web != "" {
		abs, _ := filepath.Abs(o.Web)
		if fi, err := os.Stat(filepath.Join(abs, "index.html")); err != nil || fi.IsDir() {
			return nil, fmt.Errorf("web root %s has no index.html", abs)
		}
		s.webRoot = abs
	} else if root := defaultWebRoot(cwd); root != "" {
		s.webRoot = root
	}

	return s, nil
}

func relayURL(r *relay.Relay) string {
	if r == nil {
		return ""
	}
	return r.URL
}

func relayToken(r *relay.Relay) string {
	if r == nil {
		return ""
	}
	return r.Token
}

// installPI fetches PI into the Prometheus data directory on first run, mirroring
// the original desktop's on-demand runtime install instead of shipping it.
func installPI(ctx context.Context, node string) (entry, version string, err error) {
	piDir := filepath.Join(config.Home(), "go-poc", "pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return "", "", err
	}
	sep := string(os.PathListSeparator)
	_ = os.Setenv("PATH", filepath.Dir(node)+sep+os.Getenv("PATH"))
	log.Printf("installing PI into %s (first run)", piDir)
	if err := runNPM(ctx, piDir, "install", "--no-fund", "--no-audit", "--loglevel=error",
		"@earendil-works/pi-coding-agent@1.2.0",
		"@earendil-works/pi-ai@1.2.0",
		"@earendil-works/pi-mcp@1.2.0"); err != nil {
		return "", "", err
	}
	entry, version = findPI()
	if entry == "" {
		return "", "", fmt.Errorf("PI installed but its entry point was not found")
	}
	return entry, version, nil
}

func runBootstrap(ctx context.Context) error {
	node, err := ensureNode(ctx)
	if err != nil {
		return fmt.Errorf("Node.js: %w", err)
	}
	log.Printf("Node.js: %s", node)
	sep := string(os.PathListSeparator)
	_ = os.Setenv("PATH", filepath.Dir(node)+sep+os.Getenv("PATH"))
	piDir := filepath.Join(config.Home(), "go-poc", "pi")
	if err := os.MkdirAll(piDir, 0o755); err != nil {
		return err
	}
	log.Printf("installing PI into %s", piDir)
	if err := runNPM(ctx, piDir, "install", "--no-fund", "--no-audit", "--loglevel=error",
		"@earendil-works/pi-coding-agent@1.2.0",
		"@earendil-works/pi-ai@1.2.0",
		"@earendil-works/pi-mcp@1.2.0"); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	pluginDir := ""
	for _, c := range []string{
		filepath.Join(cwd, "node", "plugin-host"),
		filepath.Join(cwd, "..", "node", "plugin-host"),
	} {
		if _, err := os.Stat(filepath.Join(c, "package.json")); err == nil {
			pluginDir = c
			break
		}
	}
	if pluginDir == "" {
		return fmt.Errorf("node/plugin-host not found")
	}
	log.Printf("installing plugin host dependencies in %s", pluginDir)
	if err := runNPM(ctx, pluginDir, "install", "--no-fund", "--no-audit", "--loglevel=error"); err != nil {
		return err
	}
	linkPIPackages(pluginDir)
	log.Printf("bundling TypeScript plugins")
	cmd := exec.CommandContext(ctx, "node", "build-plugins.mjs")
	cmd.Dir = pluginDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	log.Printf("bootstrap complete")
	return nil
}

func linkPIPackages(pluginDir string) {
	target := ""
	if dir := os.Getenv("PROMETHEUS_PI_DIR"); dir != "" {
		for _, c := range []string{
			filepath.Join(dir, "node_modules", "@earendil-works"),
			filepath.Join(dir, "@earendil-works"),
		} {
			if fi, err := os.Stat(c); err == nil && fi.IsDir() {
				target = c
				break
			}
		}
	}
	if target == "" {
		if exe, err := os.Executable(); err == nil {
			for _, c := range []string{
				filepath.Join(filepath.Dir(exe), "runtime", "pi", "node_modules", "@earendil-works"),
				filepath.Join(filepath.Dir(exe), "..", "runtime", "pi", "node_modules", "@earendil-works"),
			} {
				if fi, err := os.Stat(c); err == nil && fi.IsDir() {
					target = c
					break
				}
			}
		}
	}
	if target == "" {
		target = filepath.Join(config.Home(), "go-poc", "pi", "node_modules", "@earendil-works")
	}
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return
	}
	link := filepath.Join(pluginDir, "node_modules", "@earendil-works")
	if _, err := os.Lstat(link); err == nil {
		if _, err := os.Stat(link); err == nil {
			return
		}
		// A package copied from another machine can carry a stale absolute link.
		_ = os.Remove(link)
	}
	_ = os.MkdirAll(filepath.Dir(link), 0o755)
	if runtime.GOOS == "windows" {
		script := fmt.Sprintf("New-Item -ItemType Junction -Path '%s' -Target '%s' | Out-Null", link, target)
		_ = exec.Command("powershell", "-NoProfile", "-Command", script).Run()
		return
	}
	_ = os.Symlink(target, link)
}

func runNPM(ctx context.Context, dir string, args ...string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "cmd", append([]string{"/c", "npm"}, args...)...)
	} else {
		cmd = exec.CommandContext(ctx, "npm", args...)
	}
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func startPluginHost(node, cwd string) (string, func(), error) {
	script := ""
	candidates := []string{
		filepath.Join(cwd, "node", "plugin-host", "index.mjs"),
		filepath.Join(cwd, "..", "node", "plugin-host", "index.mjs"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "node", "plugin-host", "index.mjs"))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			script = c
			break
		}
	}
	if script == "" {
		return "", nil, os.ErrNotExist
	}
	dist := filepath.Join(filepath.Dir(script), "dist")
	if _, err := os.Stat(filepath.Join(dist, "todos.mjs")); err != nil {
		return "", nil, fmt.Errorf("plugin bundles missing in %s (run npm install + build-plugins.mjs)", dist)
	}
	// PI is installed outside the app directory by bootstrap; expose it to the
	// bundled plugins (market, modes, skills) without asking the user to run
	// bootstrap again after moving the package.
	linkPIPackages(filepath.Dir(script))
	cmd := exec.Command(node, script)
	cmd.Dir = filepath.Dir(script)
	env := append([]string(nil), os.Environ()...)
	env = append(env, "PROMETHEUS_CWD="+cwd)
	env = append(env, "PROMETHEUS_PLUGIN_DATA="+filepath.Join(config.Home(), "go-poc", "plugin-data"))
	if cfg, err := config.Load(); err == nil {
		if raw, err := json.Marshal(cfg.Gateways); err == nil {
			env = append(env, "PROMETHEUS_GATEWAYS="+string(raw))
		}
	}
	cmd.Env = env
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", nil, err
	}
	if err := cmd.Start(); err != nil {
		return "", nil, err
	}
	ready := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if strings.HasPrefix(line, "PLUGIN_HOST_READY ") {
				if port, err := strconv.Atoi(strings.TrimPrefix(line, "PLUGIN_HOST_READY ")); err == nil {
					select {
					case ready <- port:
					default:
					}
				}
			}
		}
	}()
	stop := func() {
		if runtime.GOOS == "windows" {
			_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
			return
		}
		_ = cmd.Process.Kill()
	}
	select {
	case port := <-ready:
		return fmt.Sprintf("http://127.0.0.1:%d", port), stop, nil
	case <-time.After(60 * time.Second):
		stop()
		return "", nil, fmt.Errorf("plugin host did not report READY")
	}
}

func findOpenCode() (bin, version string) {
	candidates := []string{
		filepath.Join(config.Home(), "go-poc", "opencode", "node_modules", "opencode-ai", "bin", "opencode.exe"),
		filepath.Join(config.Home(), "go-poc", "opencode", "node_modules", "opencode-ai", "bin", "opencode"),
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		candidates = append(candidates, p)
	}
	for _, c := range candidates {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c, "1.19.5"
		}
	}
	return "", ""
}

func findACP() (entry, version string) {
	entry = filepath.Join(config.Home(), "go-poc", "acp", "node_modules",
		"@zed-industries", "claude-code-acp", "dist", "index.js")
	if _, err := os.Stat(entry); err != nil {
		return "", ""
	}
	version = "0.16.1"
	pkg := filepath.Join(filepath.Dir(entry), "..", "package.json")
	if raw, err := os.ReadFile(pkg); err == nil {
		var meta struct {
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &meta) == nil && meta.Version != "" {
			version = meta.Version
		}
	}
	return entry, version
}

func findPI() (entry, version string) {
	candidates := []string{}
	if dir := os.Getenv("PROMETHEUS_PI_DIR"); dir != "" {
		candidates = append(candidates,
			filepath.Join(dir, "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
			filepath.Join(dir, "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
		)
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		candidates = append(candidates,
			filepath.Join(dir, "runtime", "pi", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
			filepath.Join(dir, "..", "runtime", "pi", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
		)
	}
	candidates = append(candidates,
		filepath.Join(config.Home(), "go-poc", "pi", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
		filepath.Join(config.Home(), "pi-agent", "node_modules", "@earendil-works", "pi-coding-agent", "dist", "bundle", "rpc-entry.js"),
	)
	for _, c := range candidates {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		version = "1.2.0"
		pkg := filepath.Join(filepath.Dir(c), "..", "..", "package.json")
		if raw, err := os.ReadFile(pkg); err == nil {
			var meta struct {
				Version string `json:"version"`
			}
			if json.Unmarshal(raw, &meta) == nil && meta.Version != "" {
				version = meta.Version
			}
		}
		return c, version
	}
	return "", ""
}

func defaultWebRoot(cwd string) string {
	candidates := []string{
		filepath.Join(cwd, "apps", "web", "dist"),
		filepath.Join(cwd, "..", "apps", "web", "dist"),
		filepath.Join(cwd, "web"),
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "web"))
	}
	for _, c := range candidates {
		if fi, err := os.Stat(filepath.Join(c, "index.html")); err == nil && !fi.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func resolveScript(path, cwd string) (string, error) {
	if filepath.IsAbs(path) {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}
	candidates := []string{
		filepath.Join(cwd, path),
		path,
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), path))
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return filepath.Abs(c)
		}
	}
	return "", fmt.Errorf("agent-host script not found: %s (use -sidecar)", path)
}

func printAgents(ctx context.Context, checkLatest bool) {
	list := agents.Detect(ctx, checkLatest)
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tVERSION\tLATEST\tUPDATE\tSOURCE\tPATH")
	for _, a := range list {
		latest, update := "-", "-"
		if a.Latest != "" {
			latest = a.Latest
			if a.UpdateAvailable {
				update = "yes"
			} else {
				update = "no"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Name, valueOr(a.Version, "-"), latest, update, a.Source, valueOr(a.Path, "-"))
	}
	_ = tw.Flush()
	for _, a := range list {
		if a.Managed != "" && !strings.EqualFold(a.Managed, a.Path) {
			fmt.Printf("  %s managed copy: %s\n", a.Name, a.Managed)
		}
		if a.Error != "" {
			fmt.Printf("  %s note: %s\n", a.Name, a.Error)
		}
	}
}

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
