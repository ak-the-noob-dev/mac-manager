package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var webFS embed.FS

type Process struct {
	ID       string  `json:"id"`
	PID      int     `json:"pid"`
	PPID     int     `json:"ppid"`
	Name     string  `json:"name"`
	Command  string  `json:"command"`
	User     string  `json:"user"`
	CPU      float64 `json:"cpu"`
	MemoryMB float64 `json:"memoryMB"`
	Uptime   string  `json:"uptime"`
	Kind     string  `json:"kind"`
	Owner    string  `json:"owner"`
	CanStop  bool    `json:"canStop"`
}

type Program struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Status   string   `json:"status"`
	Tone     string   `json:"tone"`
	Detail   string   `json:"detail"`
	Hint     string   `json:"hint,omitempty"`
	Version  string   `json:"version,omitempty"`
	CPU      float64  `json:"cpu"`
	MemoryMB float64  `json:"memoryMB"`
	Actions  []string `json:"actions"`
}

type Snapshot struct {
	Programs      []Program `json:"programs"`
	Processes     []Process `json:"processes"`
	BrewOK        bool      `json:"brewOk"`
	DockerOK      bool      `json:"dockerOk"`
	BrewMessage   string    `json:"brewMessage,omitempty"`
	DockerMessage string    `json:"dockerMessage,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

type appMeta struct {
	Name     string
	Path     string
	BundleID string
	Version  string
}

type brewService struct {
	Name   string
	Status string
	User   string
	File   string
}

type dockerContainer struct {
	ID     string
	Name   string
	State  string
	Status string
	Image  string
}

type catalog struct {
	Apps        []appMeta
	Versions    map[string]string
	Leaves      map[string]bool
	Casks       map[string]string
	BrewMessage string
	At          time.Time
}

var (
	dockerIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{12,64}$`)

	catalogMu      sync.Mutex
	catalogCache   catalog
	catalogGen     int
	catalogLoading chan struct{}
)

func command(ctx context.Context, name string, args ...string) (string, error) {
	c := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	err := c.Run()
	if err != nil {
		if ctx.Err() != nil {
			return stdout.String(), ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), errors.New(msg)
	}
	return stdout.String(), nil
}

func lines(out string) []string {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	parts := strings.Split(out, "\n")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			result = append(result, part)
		}
	}
	return result
}

func cleanErr(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "The command timed out"
	}
	msg := strings.TrimSpace(err.Error())
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "executable file not found"):
		return "Not installed"
	case strings.Contains(low, "docker daemon") || strings.Contains(low, "cannot connect to the docker") || strings.Contains(low, "failed to connect to the docker"):
		return "Docker isn't running"
	}
	msg = strings.Join(strings.Fields(msg), " ")
	if len(msg) > 280 {
		msg = msg[:280] + "…"
	}
	return msg
}

func validBrewName(name string) bool {
	if name == "" || len(name) > 120 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@._+-", r)) {
			return false
		}
	}
	return true
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return os.Getenv("USER")
}

func processes() ([]Process, error) {
	me := currentUsername()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := command(ctx, "ps", "-axo", "pid=,ppid=,user=,%cpu=,rss=,etime=,command=")
	if err != nil {
		return nil, err
	}
	items := make([]Process, 0)
	self := os.Getpid()
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 7 {
			continue
		}
		pid, _ := strconv.Atoi(f[0])
		ppid, _ := strconv.Atoi(f[1])
		cpu, _ := strconv.ParseFloat(f[3], 64)
		rss, _ := strconv.ParseFloat(f[4], 64)
		cmd := strings.Join(f[6:], " ")
		name := filepath.Base(f[6])
		kind := "Process"
		lc := strings.ToLower(cmd)
		switch {
		case strings.Contains(lc, "docker"):
			kind = "Docker"
		case strings.Contains(lc, "homebrew") || strings.Contains(lc, "/opt/homebrew") || strings.Contains(lc, "/usr/local/cellar"):
			kind = "Homebrew"
		case strings.Contains(lc, ".app/"):
			kind = "App"
		}
		owner := "macOS / background"
		if f[2] == me {
			owner = "You"
		}
		items = append(items, Process{
			ID:       fmt.Sprintf("process:%d", pid),
			PID:      pid,
			PPID:     ppid,
			Name:     name,
			Command:  cmd,
			User:     f[2],
			CPU:      cpu,
			MemoryMB: rss / 1024,
			Uptime:   f[5],
			Kind:     kind,
			Owner:    owner,
			CanStop:  owner == "You" && pid > 1 && pid != self,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].CPU+items[i].MemoryMB/1000 > items[j].CPU+items[j].MemoryMB/1000
	})
	return items, nil
}

func belongsToApp(command, appPath string) bool {
	if command == "" || appPath == "" {
		return false
	}
	return strings.Contains(command, appPath+"/")
}

func appRoots() []string {
	roots := []string{"/Applications"}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		roots = append(roots, filepath.Join(home, "Applications"))
	}
	return roots
}

func allowedAppPath(p string) bool {
	p = filepath.Clean(p)
	if !strings.HasSuffix(p, ".app") {
		return false
	}
	info, err := os.Stat(p)
	if err != nil || !info.IsDir() {
		return false
	}
	base := filepath.Base(p)
	for _, root := range appRoots() {
		if filepath.Join(root, base) == p {
			return true
		}
	}
	return false
}

func inspectApp(path string) (appMeta, bool) {
	name := strings.TrimSuffix(filepath.Base(path), ".app")
	meta := appMeta{Name: name, Path: path}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := command(ctx, "plutil", "-convert", "json", "-o", "-", filepath.Join(path, "Contents", "Info.plist"))
	if err != nil {
		return meta, true
	}
	var raw map[string]any
	if json.Unmarshal([]byte(out), &raw) != nil {
		return meta, true
	}
	bundleID := asString(raw["CFBundleIdentifier"])
	if strings.HasPrefix(strings.ToLower(bundleID), "com.apple.") {
		return appMeta{}, false
	}
	if display := asString(raw["CFBundleDisplayName"]); display != "" {
		meta.Name = display
	} else if bundleName := asString(raw["CFBundleName"]); bundleName != "" {
		meta.Name = bundleName
	}
	meta.Version = asString(raw["CFBundleShortVersionString"])
	meta.BundleID = bundleID
	return meta, true
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

func scanApps() []appMeta {
	var found []string
	for _, root := range appRoots() {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if strings.HasSuffix(entry.Name(), ".app") {
				found = append(found, filepath.Join(root, entry.Name()))
			}
		}
	}
	apps := make([]appMeta, 0, len(found))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, path := range found {
		wg.Add(1)
		sem <- struct{}{}
		go func(path string) {
			defer wg.Done()
			defer func() { <-sem }()
			meta, ok := inspectApp(path)
			if !ok {
				return
			}
			mu.Lock()
			apps = append(apps, meta)
			mu.Unlock()
		}(path)
	}
	wg.Wait()
	sort.Slice(apps, func(i, j int) bool { return strings.ToLower(apps[i].Name) < strings.ToLower(apps[j].Name) })
	return apps
}

func loadBrewPackages() (map[string]string, map[string]bool, map[string]string, string) {
	versions := map[string]string{}
	leaves := map[string]bool{}
	casks := map[string]string{}
	if _, err := exec.LookPath("brew"); err != nil {
		return versions, leaves, casks, "Homebrew is not installed"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	out, err := command(ctx, "brew", "leaves")
	if err != nil {
		return versions, leaves, casks, "Homebrew: " + cleanErr(err)
	}
	for _, line := range lines(out) {
		leaves[line] = true
	}
	if out, err = command(ctx, "brew", "list", "--versions"); err == nil {
		for _, line := range lines(out) {
			name, ver, _ := strings.Cut(line, " ")
			if name != "" {
				versions[name] = ver
			}
		}
	}
	out, err = command(ctx, "brew", "list", "--cask", "--versions")
	if err != nil {
		out, err = command(ctx, "brew", "list", "--cask", "-1")
	}
	if err == nil {
		for _, line := range lines(out) {
			name, ver, _ := strings.Cut(line, " ")
			if name != "" {
				casks[name] = ver
			}
		}
	}
	return versions, leaves, casks, ""
}

func loadCatalog() catalog {
	cat := catalog{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cat.Apps = scanApps()
	}()
	cat.Versions, cat.Leaves, cat.Casks, cat.BrewMessage = loadBrewPackages()
	wg.Wait()
	return cat
}

func catalogReady(now time.Time) bool {
	return !catalogCache.At.IsZero() && now.Sub(catalogCache.At) < 45*time.Second
}

func getCatalog(force bool) catalog {
	catalogMu.Lock()
	if force {
		catalogGen++
		catalogCache.At = time.Time{}
	}
	for {
		if catalogReady(time.Now()) {
			cat := catalogCache
			catalogMu.Unlock()
			return cat
		}
		if catalogLoading != nil {
			ch := catalogLoading
			catalogMu.Unlock()
			<-ch
			catalogMu.Lock()
			continue
		}
		gen := catalogGen
		ch := make(chan struct{})
		catalogLoading = ch
		catalogMu.Unlock()

		cat := loadCatalog()
		cat.At = time.Now()

		catalogMu.Lock()
		if catalogGen == gen {
			catalogCache = cat
		}
		catalogLoading = nil
		close(ch)
		if catalogGen == gen {
			catalogMu.Unlock()
			return cat
		}
	}
}

func invalidateCatalog() {
	catalogMu.Lock()
	catalogGen++
	catalogCache.At = time.Time{}
	catalogMu.Unlock()
}

func brewServices() ([]brewService, error) {
	if _, err := exec.LookPath("brew"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := command(ctx, "brew", "services", "list")
	if err != nil {
		return nil, err
	}
	result := make([]brewService, 0)
	for _, line := range lines(out) {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "Name" {
			continue
		}
		svc := brewService{Name: f[0], Status: f[1]}
		if len(f) >= 3 {
			svc.User = f[2]
		}
		if len(f) >= 4 {
			svc.File = strings.Join(f[3:], " ")
		}
		result = append(result, svc)
	}
	return result, nil
}

func serviceControls(status string) (string, string, []string) {
	switch status {
	case "started", "running":
		return "Running", "ok", []string{"restart", "stop"}
	case "error":
		return "Error", "bad", []string{"start", "restart", "stop"}
	case "stopped", "none", "":
		return "Stopped", "warn", []string{"start"}
	default:
		return status, "warn", []string{"start", "stop"}
	}
}

func dockerContainers() ([]dockerContainer, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, err := command(ctx, "docker", "ps", "-a", "--format", "{{.ID}}\t{{.Names}}\t{{.State}}\t{{.Status}}\t{{.Image}}")
	if err != nil {
		return nil, err
	}
	result := make([]dockerContainer, 0)
	for _, line := range lines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 5 {
			continue
		}
		result = append(result, dockerContainer{ID: f[0], Name: f[1], State: f[2], Status: f[3], Image: f[4]})
	}
	return result, nil
}

func dockerControls(state string) (string, string, []string) {
	switch state {
	case "running":
		return "Running", "ok", []string{"restart", "stop"}
	case "paused":
		return "Paused", "warn", []string{"start", "stop"}
	case "restarting":
		return "Restarting", "warn", []string{"stop"}
	default:
		return "Stopped", "warn", []string{"start"}
	}
}

func programFromApp(meta appMeta, procs []Process) Program {
	var cpu, mem float64
	running := false
	canQuit := false
	for _, proc := range procs {
		if !belongsToApp(proc.Command, meta.Path) {
			continue
		}
		running = true
		if proc.Owner == "You" {
			cpu += proc.CPU
			mem += proc.MemoryMB
			if proc.CanStop {
				canQuit = true
			}
		}
	}
	actions := []string{"open"}
	status, tone := "Not running", "muted"
	if running {
		status, tone = "Running", "ok"
		if canQuit {
			actions = append(actions, "quit")
		}
	}
	detail := meta.Version
	if detail == "" {
		detail = "Application"
	}
	return Program{
		ID:       "app:" + meta.Path,
		Name:     meta.Name,
		Kind:     "App",
		Status:   status,
		Tone:     tone,
		Detail:   detail,
		Hint:     meta.Path,
		Version:  meta.Version,
		CPU:      cpu,
		MemoryMB: mem,
		Actions:  actions,
	}
}

func versionFor(versions map[string]string, name string) string {
	if versions == nil || name == "" {
		return ""
	}
	if ver := versions[name]; ver != "" {
		return ver
	}
	if slash := strings.LastIndex(name, "/"); slash >= 0 {
		return versions[name[slash+1:]]
	}
	return ""
}

func programFromBrew(name string, cat catalog, svc brewService, isService bool) Program {
	ver := versionFor(cat.Versions, name)
	p := Program{
		ID:      "formula:" + name,
		Name:    name,
		Kind:    "Formula",
		Status:  "Installed",
		Tone:    "muted",
		Version: ver,
		Detail:  ver,
		Actions: []string{},
	}
	if p.Detail == "" {
		p.Detail = "Homebrew formula"
	}
	if isService {
		p.Kind = "Service"
		p.Status, p.Tone, p.Actions = serviceControls(svc.Status)
		parts := make([]string, 0, 3)
		if ver != "" {
			parts = append(parts, ver)
		}
		if svc.User != "" {
			parts = append(parts, svc.User)
		}
		if svc.File != "" {
			parts = append(parts, filepath.Base(svc.File))
		}
		if len(parts) > 0 {
			p.Detail = strings.Join(parts, " · ")
		}
		p.Hint = svc.File
	}
	if cat.Leaves[name] {
		p.Actions = append(p.Actions, "uninstall")
	}
	if p.Actions == nil {
		p.Actions = []string{}
	}
	return p
}

func buildPrograms(cat catalog, procs []Process, svcs []brewService, containers []dockerContainer) []Program {
	programs := make([]Program, 0)
	for _, app := range cat.Apps {
		programs = append(programs, programFromApp(app, procs))
	}
	byName := make(map[string]brewService, len(svcs))
	names := make(map[string]struct{})
	for name := range cat.Leaves {
		names[name] = struct{}{}
	}
	for _, svc := range svcs {
		byName[svc.Name] = svc
		names[svc.Name] = struct{}{}
	}
	brewNames := make([]string, 0, len(names))
	for name := range names {
		brewNames = append(brewNames, name)
	}
	sort.Strings(brewNames)
	for _, name := range brewNames {
		svc, isService := byName[name]
		programs = append(programs, programFromBrew(name, cat, svc, isService))
	}
	caskNames := make([]string, 0, len(cat.Casks))
	for name := range cat.Casks {
		caskNames = append(caskNames, name)
	}
	sort.Strings(caskNames)
	for _, name := range caskNames {
		ver := cat.Casks[name]
		detail := ver
		if detail == "" {
			detail = "Homebrew cask"
		}
		programs = append(programs, Program{
			ID:      "cask:" + name,
			Name:    name,
			Kind:    "Cask",
			Status:  "Installed",
			Tone:    "muted",
			Version: ver,
			Detail:  detail,
			Actions: []string{"uninstall"},
		})
	}
	for _, container := range containers {
		status, tone, actions := dockerControls(container.State)
		detail := container.Image
		if container.Status != "" {
			if detail != "" {
				detail += " · "
			}
			detail += container.Status
		}
		programs = append(programs, Program{
			ID:      "docker:" + container.ID,
			Name:    container.Name,
			Kind:    "Docker",
			Status:  status,
			Tone:    tone,
			Detail:  detail,
			Hint:    container.ID,
			Actions: actions,
		})
	}
	sort.SliceStable(programs, func(i, j int) bool {
		return programLess(programs[i], programs[j])
	})
	return programs
}

func toneRank(tone string) int {
	switch tone {
	case "bad":
		return 0
	case "ok":
		return 1
	case "warn":
		return 2
	default:
		return 3
	}
}

func programLess(a, b Program) bool {
	if ra, rb := toneRank(a.Tone), toneRank(b.Tone); ra != rb {
		return ra < rb
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return strings.ToLower(a.Name) < strings.ToLower(b.Name)
}

func snapshot(fresh bool) (Snapshot, error) {
	var (
		procs      []Process
		procErr    error
		svcs       []brewService
		svcErr     error
		containers []dockerContainer
		dockerErr  error
		wg         sync.WaitGroup
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		procs, procErr = processes()
	}()
	go func() {
		defer wg.Done()
		svcs, svcErr = brewServices()
	}()
	go func() {
		defer wg.Done()
		containers, dockerErr = dockerContainers()
	}()
	wg.Wait()
	if procErr != nil {
		return Snapshot{}, procErr
	}
	cat := getCatalog(fresh)
	programs := buildPrograms(cat, procs, svcs, containers)
	snap := Snapshot{
		Programs:  programs,
		Processes: procs,
		UpdatedAt: time.Now(),
		DockerOK:  dockerErr == nil,
	}
	if cat.BrewMessage != "" {
		snap.BrewMessage = cat.BrewMessage
	} else if svcErr != nil {
		snap.BrewMessage = cleanErr(svcErr)
	}
	snap.BrewOK = snap.BrewMessage == ""
	if dockerErr != nil {
		snap.DockerMessage = cleanErr(dockerErr)
	}
	return snap, nil
}

func openApp(ctx context.Context, path string) error {
	if !allowedAppPath(path) {
		return errors.New("That app is not available")
	}
	_, err := command(ctx, "open", path)
	return err
}

func quitApp(path string) error {
	if !allowedAppPath(path) {
		return errors.New("That app is not available")
	}
	items, err := processes()
	if err != nil {
		return err
	}
	matched := 0
	failed := 0
	for _, item := range items {
		if !belongsToApp(item.Command, path) || !item.CanStop {
			continue
		}
		matched++
		if syscall.Kill(item.PID, syscall.SIGTERM) != nil {
			failed++
		}
	}
	if matched == 0 {
		return errors.New("That app is not running under your account")
	}
	if failed == matched {
		return errors.New("Could not quit that app")
	}
	return nil
}

func stopProcess(id string) error {
	pid, err := strconv.Atoi(strings.TrimPrefix(id, "process:"))
	if err != nil || pid <= 1 || pid == os.Getpid() {
		return errors.New("That process cannot be stopped")
	}
	items, err := processes()
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.PID == pid && item.CanStop {
			return syscall.Kill(pid, syscall.SIGTERM)
		}
	}
	return errors.New("Only processes owned by your account can be stopped")
}

func brewIsLeaf(ctx context.Context, name string) bool {
	out, err := command(ctx, "brew", "leaves")
	if err != nil {
		return false
	}
	for _, line := range lines(out) {
		if line == name {
			return true
		}
	}
	return false
}

func brewIsCask(ctx context.Context, name string) bool {
	out, err := command(ctx, "brew", "list", "--cask", "-1")
	if err != nil {
		return false
	}
	for _, line := range lines(out) {
		if line == name {
			return true
		}
	}
	return false
}

func brewAction(ctx context.Context, kind, name, action string) error {
	if !validBrewName(name) {
		return errors.New("Invalid Homebrew package")
	}
	switch action {
	case "start", "stop", "restart":
		if kind != "formula" {
			return errors.New("Only Homebrew services can be started")
		}
		_, err := command(ctx, "brew", "services", action, name)
		return err
	case "uninstall":
		if kind == "cask" {
			if !brewIsCask(ctx, name) {
				return errors.New("That cask is not installed")
			}
			_, err := command(ctx, "brew", "uninstall", "--cask", name)
			return err
		}
		if !brewIsLeaf(ctx, name) {
			return errors.New("Only packages you installed can be removed")
		}
		_, err := command(ctx, "brew", "uninstall", name)
		return err
	default:
		return errors.New("Unknown Homebrew action")
	}
}

func dockerAction(ctx context.Context, id, action string) error {
	if !dockerIDPattern.MatchString(id) {
		return errors.New("Invalid container")
	}
	switch action {
	case "start":
		out, err := command(ctx, "docker", "inspect", "-f", "{{.State.Status}}", id)
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) == "paused" {
			_, err = command(ctx, "docker", "unpause", id)
			return err
		}
		_, err = command(ctx, "docker", "start", id)
		return err
	case "stop":
		_, err := command(ctx, "docker", "stop", id)
		return err
	case "restart":
		_, err := command(ctx, "docker", "restart", id)
		return err
	default:
		return errors.New("Unknown Docker action")
	}
}

func perform(ctx context.Context, id, action string) error {
	switch {
	case strings.HasPrefix(id, "process:"):
		if action != "stop" {
			return errors.New("Unknown process action")
		}
		return stopProcess(id)
	case strings.HasPrefix(id, "app:"):
		path := strings.TrimPrefix(id, "app:")
		switch action {
		case "open":
			return openApp(ctx, path)
		case "quit":
			return quitApp(path)
		default:
			return errors.New("Unknown app action")
		}
	case strings.HasPrefix(id, "formula:"):
		return brewAction(ctx, "formula", strings.TrimPrefix(id, "formula:"), action)
	case strings.HasPrefix(id, "cask:"):
		return brewAction(ctx, "cask", strings.TrimPrefix(id, "cask:"), action)
	case strings.HasPrefix(id, "docker:"):
		return dockerAction(ctx, strings.TrimPrefix(id, "docker:"), action)
	default:
		return errors.New("Unknown item")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func main() {
	var mu sync.Mutex
	ui, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "GET required"})
			return
		}
		s, err := snapshot(r.URL.Query().Get("fresh") == "1")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": cleanErr(err)})
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("/api/action", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			ID     string `json:"id"`
			Action string `json:"action"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || req.ID == "" || req.Action == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request"})
			return
		}
		timeout := 20 * time.Second
		if req.Action == "uninstall" {
			timeout = 60 * time.Second
		}
		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if err := perform(ctx, req.ID, req.Action); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": cleanErr(err)})
			return
		}
		if req.Action == "uninstall" {
			invalidateCatalog()
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Done"})
	})
	mux.Handle("/", http.FileServer(http.FS(ui)))

	port := "8787"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	server := &http.Server{
		Addr:              "127.0.0.1:" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Mac Manager is running at http://localhost:%s", port)
	log.Fatal(server.ListenAndServe())
}
