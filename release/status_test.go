package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The manifest reads as the updater's awk reads it: the canary's hosts get the second section, everyone else the top,
// and a manifest cut short at any line is refused whole.
func TestTheManifestReadsAsTheUpdaterReadsIt(t *testing.T) {
	canary := "canary Cloud Sun1\n" + manifestOf(commit("a")) + manifestOf(commit("b"))
	manifest, err := ParseManifest(canary)
	if err != nil || manifest.Top() != commit("a") || manifest.For("cloud") != commit("b") || manifest.For("Server") != commit("a") || manifest.CanaryVersion() != commit("b") {
		t.Fatalf("%+v, %v", manifest, err)
	}
	lines := strings.SplitAfter(canary, "\n")
	for cut := 1; cut < len(lines)-1; cut++ {
		if _, err := ParseManifest(strings.Join(lines[:cut], "")); err == nil {
			t.Errorf("cut after line %d read", cut)
		}
	}
	for name, text := range map[string]string{
		"a canary line not first": manifestOf(commit("a")) + "canary Cloud\n",
		"a wrong count":           strings.Replace(manifestOf(commit("a")), "end 2", "end 3", 1),
		"a name twice":            "version " + commit("a") + "\nloom linux/amd64 " + strings.Repeat("1", 64) + "\nloom linux/amd64 " + strings.Repeat("2", 64) + "\nend 2\n",
		"a short sha256":          "version " + commit("a") + "\nloom linux/amd64 1234\nend 1\n",
		"a stray line":            manifestOf(commit("a")) + "hello\n",
		"three sections":          "canary Cloud\n" + manifestOf(commit("a")) + manifestOf(commit("b")) + manifestOf(commit("c")),
	} {
		if _, err := ParseManifest(text); err == nil {
			t.Errorf("%s: read", name)
		}
	}
}

// The receiver keeps each known box's latest report, stamped with when it arrived and where from, and refuses an
// unknown host, a bad name and anything that isn't a report.
func TestTheReceiverKeepsEachBoxsLatestReport(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)
	receiver := &Receiver{Directory: directory, Hosts: []string{"Workshop", "Cloud"}, Secret: testSecret, Addresses: map[string][]string{"cloud": {"10.10.102.20"}},
		Now: func() time.Time { return now }}
	post := func(method, path, body string) int {
		recorder := httptest.NewRecorder()
		request := signed(reportToken(t, "Cloud"), []byte(body), "10.10.102.20")
		request.Method, request.URL.Path = method, path
		receiver.ServeHTTP(recorder, request)
		return recorder.Code
	}
	if code := post(http.MethodPost, "/report", `{"host":"cloud","version":"`+commit("a")+`","previous":"","at":"2026-10-10T15:59:59Z"}`); code != http.StatusNoContent {
		t.Fatalf("answered %d", code)
	}
	report, err := ReadReport(directory, "Cloud")
	if err != nil || report == nil || report.Version != commit("a") || !report.Received.Equal(now) || report.From != "10.10.102.20" {
		t.Fatalf("%+v, %v", report, err)
	}
	for name, test := range map[string][3]string{
		"an unknown host":  {http.MethodPost, "/report", `{"host":"Sun1","version":"x"}`},
		"a host with a /":  {http.MethodPost, "/report", `{"host":"../cloud","version":"x"}`},
		"no host":          {http.MethodPost, "/report", `{"version":"x"}`},
		"not JSON":         {http.MethodPost, "/report", `version x`},
		"a GET":            {http.MethodGet, "/report", ``},
		"another path":     {http.MethodPost, "/reports", `{"host":"Cloud"}`},
		"a report too big": {http.MethodPost, "/report", `{"host":"Cloud","refused":"` + strings.Repeat("x", 70<<10) + `"}`},
	} {
		if code := post(test[0], test[1], test[2]); code/100 == 2 {
			t.Errorf("%s: answered %d", name, code)
		}
	}
	if entries, _ := os.ReadDir(directory); len(entries) != 1 {
		t.Fatalf("files kept: %v", entries)
	}
}

// Anything on the house's network reaches the receiver, so a report counts only when its own box signed it, with
// the report token minted for it from the token secret, and sent it from its own address, timed now and newer than
// its last. Each other report is refused and leaves the box's kept report as it was.
func TestTheReceiverTakesOnlyWhatABoxSignedFromItsAddress(t *testing.T) {
	directory := t.TempDir()
	now := time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)
	var log bytes.Buffer
	receiver := &Receiver{Directory: directory, Hosts: []string{"Workshop", "Cloud"}, Secret: testSecret, Addresses: map[string][]string{"cloud": {"10.10.102.20", "fd00::20"}},
		Resolve: func(_ context.Context, host string) ([]string, error) {
			if host == "Workshop" {
				return []string{"10.10.102.10"}, nil
			}
			return nil, errors.New("no such host")
		},
		Now: func() time.Time { return now }, Log: &log}
	body := func(host, version string, at time.Time) []byte {
		content, _ := json.Marshal(Report{Host: host, Version: version, Hooked: version, Services: []string{serveUp}, At: at.Format(time.RFC3339)})
		return content
	}
	send := func(request *http.Request) int {
		recorder := httptest.NewRecorder()
		receiver.ServeHTTP(recorder, request)
		return recorder.Code
	}
	cloud, workshop := reportToken(t, "Cloud"), reportToken(t, "Workshop")
	accepted := signed(cloud, body("Cloud", commit("a"), now), "10.10.102.20")
	replay := signed(cloud, body("Cloud", commit("a"), now), "10.10.102.20")
	if code := send(accepted); code != http.StatusNoContent {
		t.Fatalf("Cloud's own report: %d", code)
	}
	if code := send(signed(workshop, body("Workshop", commit("a"), now), "10.10.102.10")); code != http.StatusNoContent {
		t.Fatalf("Workshop's own report, from the address its name resolves to: %d", code)
	}
	otherSecret, _ := MintReportToken([]byte("another house's secret"), "Cloud", now.Add(time.Hour))
	expired, _ := MintReportToken(testSecret, "Cloud", now.Add(-time.Second))
	unsigned := httptest.NewRequest(http.MethodPost, "/report", bytes.NewReader(body("Cloud", commit("b"), now.Add(time.Second))))
	unsigned.RemoteAddr = "10.10.102.20:41234"
	tampered := signed(cloud, body("Cloud", commit("a"), now.Add(time.Second)), "10.10.102.20")
	tampered.Body = io.NopCloser(bytes.NewReader(body("Cloud", commit("b"), now.Add(time.Second))))
	wrongClaims := signed(cloud, body("Cloud", commit("b"), now.Add(time.Second)), "10.10.102.20")
	claims, _, _ := strings.Cut(workshop, ".")
	wrongClaims.Header.Set(ClaimsHeader, claims)
	for name, test := range map[string]struct {
		request *http.Request
		code    int
	}{
		"unsigned":                              {unsigned, http.StatusUnauthorized},
		"signed by another box's token":         {signed(workshop, body("Cloud", commit("b"), now.Add(time.Second)), "10.10.102.20"), http.StatusUnauthorized},
		"signed by another secret's token":      {signed(otherSecret, body("Cloud", commit("b"), now.Add(time.Second)), "10.10.102.20"), http.StatusUnauthorized},
		"signed by an expired token":            {signed(expired, body("Cloud", commit("b"), now.Add(time.Second)), "10.10.102.20"), http.StatusUnauthorized},
		"changed after it was signed":           {tampered, http.StatusUnauthorized},
		"claims another box's token":            {wrongClaims, http.StatusUnauthorized},
		"sent from another address":             {signed(cloud, body("Cloud", commit("b"), now.Add(time.Second)), "10.66.66.66"), http.StatusForbidden},
		"from a box whose name doesn't resolve": {signed(workshop, body("Workshop", commit("b"), now.Add(time.Second)), "10.10.102.20"), http.StatusForbidden},
		"replayed":                              {replay, http.StatusConflict},
		"timed long ago":                        {signed(cloud, body("Cloud", commit("b"), now.Add(-time.Hour)), "10.10.102.20"), http.StatusForbidden},
		"timed in the future":                   {signed(cloud, body("Cloud", commit("b"), now.Add(time.Hour)), "10.10.102.20"), http.StatusForbidden},
	} {
		if code := send(test.request); code != test.code {
			t.Errorf("%s: answered %d, want %d", name, code, test.code)
		}
	}
	if report, err := ReadReport(directory, "Cloud"); err != nil || report.Version != commit("a") {
		t.Fatalf("a refused report was kept: %+v, %v", report, err)
	}
	if !strings.Contains(log.String(), "refused a report from 10.66.66.66:41234: a report for Cloud came from 10.66.66.66") {
		t.Fatalf("a refusal isn't named:\n%s", log.String())
	}
	// A newer report of its own, from its other address, is taken.
	if code := send(signed(cloud, body("Cloud", commit("b"), now.Add(time.Second)), "fd00::20")); code != http.StatusNoContent {
		t.Fatalf("Cloud's next report: %d", code)
	}
}

// A probe line reads as its service, state, substate and restarts.
func TestAProbeLineReadsAsAService(t *testing.T) {
	if service := ParseService("loom-serve.service active running restarts=3"); service != (Service{"loom-serve.service", "active", "running", 3}) {
		t.Fatalf("%+v", service)
	}
	if service := ParseService("loom-plan.service not-found"); service != (Service{"loom-plan.service", "not-found", "", -1}) {
		t.Fatalf("%+v", service)
	}
	report := Report{Services: []string{"loom-serve.service active running restarts=0", "loom-pusher.timer active waiting restarts=0", "loom-plan.service failed failed restarts=9"}}
	if problems := report.Unhealthy([]string{"loom-serve.service", "loom-judge.service"}); !reflect.DeepEqual(problems, []string{"loom-judge.service not reported", "loom-plan.service failed failed"}) {
		t.Fatalf("%q", problems)
	}
}

// Status names each box's state plainly: a box behind the release lags, a held one says so and isn't called lagging, a
// quiet one is silent, and one with a service down, failing hooks or a serve that stopped asking says that.
func TestStatusFlagsALaggingBox(t *testing.T) {
	w := newWorld(t)
	now := w.now
	published, _ := ParseManifest("canary Cloud\n" + manifestOf(commit("a")) + manifestOf(commit("b")))
	w.report("Workshop", commit("a"), commit("a"), "loom-plan.service active running restarts=0", "loom-release.service active running restarts=1")
	w.report("Cloud", commit("b"), commit("b"), serveUp)
	w.report("Server", "8"+commit("2")[1:], "8"+commit("2")[1:], serveUp)
	w.reportHeld("Home", commit("c"), commit("c"))
	w.report("Chonchon", commit("a"), commit("9"), "loom-serve.service activating auto-restart restarts=40")
	asked := map[string]time.Time{"cloud": now, "server": now.Add(-time.Minute), "chonchon": now.Add(-time.Hour)}
	boxes, err := Boxes(w.config, published, asked, now.Add(30*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]string{}
	for _, box := range boxes {
		verdicts[box.Box] = strings.Join(append(box.Problems, box.Notes...), "; ")
	}
	want := map[string]string{
		"Workshop": "",
		"Cloud":    "",
		"Server":   "LAGS: runs 822222222222, the release for it is aaaaaaaaaaaa",
		"Home":     "HELD at cccccccccccc by its update.conf (the release for it is aaaaaaaaaaaa)",
	}
	for box, verdict := range want {
		if verdicts[box] != verdict {
			t.Errorf("%s: %q, want %q", box, verdicts[box], verdict)
		}
	}
	for _, part := range []string{"HOOKS FAILING: they last passed for 999999999999", "UNHEALTHY: loom-serve.service activating auto-restart", "NOT ASKING: its serve last asked "} {
		if !strings.Contains(verdicts["Chonchon"], part) {
			t.Errorf("Chonchon: %q lacks %q", verdicts["Chonchon"], part)
		}
	}
	var out bytes.Buffer
	if problem := WriteStatus(&out, published, State{Phase: PhaseSoak, Commit: commit("b"), Since: now}, boxes, now.Add(30*time.Second)); !problem {
		t.Fatalf("no problem said:\n%s", out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "Server ") != strings.Contains(line, "LAGS") {
			t.Errorf("a line lags or doesn't wrongly: %q", line)
		}
	}
	if !strings.Contains(out.String(), "canary bbbbbbbbbbbb for Cloud") || !strings.Contains(out.String(), "soak of bbbbbbbbbbbb") {
		t.Fatalf("the release and the watcher:\n%s", out.String())
	}
	// Twelve minutes on, every box is silent; a box that never reported says how to make it.
	later, _ := Boxes(w.config, published, nil, now.Add(13*time.Minute))
	for _, box := range later {
		if !strings.Contains(strings.Join(box.Problems, " "), "SILENT for 13m") {
			t.Errorf("%s: %q", box.Box, box.Problems)
		}
	}
	os.Remove(reportPath(filepath.Join(w.config.State, "reports"), "Server"))
	never, _ := Boxes(w.config, published, nil, now)
	if !strings.Contains(strings.Join(never[2].Problems, " "), "NEVER REPORTED") {
		t.Fatalf("%+v", never[2])
	}
	// All well: no problem said.
	healthy := newWorld(t)
	for _, box := range healthy.config.Boxes {
		healthy.report(box, commit("a"), commit("a"))
	}
	boxes, _ = Boxes(healthy.config, healthy.published(), nil, healthy.now)
	if WriteStatus(io.Discard, healthy.published(), State{Phase: PhaseDone, Commit: commit("a")}, boxes, healthy.now) {
		t.Fatalf("a problem said of a fleet all on its release: %+v", boxes)
	}
}

func TestReleaseConfReadsAndRefuses(t *testing.T) {
	defaults := DefaultConfig("/home/ahra")
	config, err := ReadConfig("# Workshop\ncanary = Server\nboxes = Workshop, Server Home\nsoak = 20m\nrestarts = 0\nunits = loom-plan.service loom-pusher.timer\n"+
		"listen = 10.10.102.10:7381\naddresses = Server=10.10.102.30,fd00::30 Home=10.10.102.40\n", defaults)
	if err != nil || config.Canary != "Server" || !reflect.DeepEqual(config.Boxes, []string{"Workshop", "Server", "Home"}) || config.Soak != 20*time.Minute || config.Restarts != 0 ||
		config.Out != "/home/ahra/loom-releases/out" || len(config.Units) != 2 || config.Listen != "10.10.102.10:7381" ||
		!reflect.DeepEqual(config.Addresses, map[string][]string{"server": {"10.10.102.30", "fd00::30"}, "home": {"10.10.102.40"}}) {
		t.Fatalf("%+v, %v", config, err)
	}
	for name, text := range map[string]string{
		"an unknown key":         "canry = Cloud\n",
		"a key twice":            "soak = 10m\nsoak = 11m\n",
		"a canary not a box":     "canary = Sun1\n",
		"a soak under 6m":        "soak = 2m\n",
		"a bad duration":         "canary-within = soon\n",
		"a unit with no kind":    "units = loom-plan\n",
		"a negative restarts":    "restarts = -1\n",
		"an empty value":         "base =\n",
		"a line with no equals":  "canary Cloud\n",
		"a box with a slash":     "boxes = Cloud a/b\n",
		"a canary with a space?": "canary = Cl oud\n",
		"listen on every port":   "listen = :7381\n",
		"listen on every IPv4":   "listen = 0.0.0.0:7381\n",
		"listen on every IPv6":   "listen = [::]:7381\n",
		"listen on a name":       "listen = workshop:7381\n",
		"listen with no port":    "listen = 10.10.102.10\n",
		"an address not an IP":   "addresses = Cloud=cloud.lan\n",
		"an address with no box": "addresses = 10.10.102.20\n",
	} {
		if config, err := ReadConfig(text, defaults); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
	if order, err := ParseOrder("# none\n\nworkers after fleet # the Go judge first\nfleet after schema\n"); err != nil || !reflect.DeepEqual(order, Order{Before: []string{"schema"}, After: []string{"workers"}}) {
		t.Fatalf("%+v, %v", order, err)
	}
	for _, text := range []string{"workers before fleet\n", "judge after workers\n", "fleet after fleet\n", "workers after fleet\nworkers after fleet\n", "Workers after fleet\n", "deploy the workers\n"} {
		if order, err := ParseOrder(text); err == nil {
			t.Errorf("%q read as %+v", text, order)
		}
	}
}

// The watcher's real steps: git fetches the branch's head into the clone, says whether it descends from the fleet's
// release, and reads its order file (none is no order); Promote makes a commit's own manifest current.txt.
func TestTheStepsOnWorkshop(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	directory := t.TempDir()
	git := func(directory string, arguments ...string) string {
		command := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	origin, clone := filepath.Join(directory, "origin"), filepath.Join(directory, "clone")
	os.MkdirAll(origin, 0o755)
	git(origin, "init", "-q", "-b", "main")
	git(origin, "commit", "-q", "--allow-empty", "-m", "first")
	first := git(origin, "rev-parse", "HEAD")
	git(directory, "clone", "-q", origin, clone)
	os.MkdirAll(filepath.Join(origin, "updater"), 0o755)
	os.WriteFile(filepath.Join(origin, OrderFile), []byte("workers after fleet\n"), 0o644)
	git(origin, "add", "-A")
	git(origin, "commit", "-q", "-m", "second")
	second := git(origin, "rev-parse", "HEAD")
	config := DefaultConfig(directory)
	config.Repository, config.Out = clone, filepath.Join(directory, "out")
	steps := Commands(config, io.Discard)
	head, err := steps.Head(context.Background())
	if err != nil || head != second {
		t.Fatalf("head %q, %v (want %s)", head, err, second)
	}
	if descends, err := steps.Descends(context.Background(), first, second); err != nil || !descends {
		t.Fatalf("second from first: %v, %v", descends, err)
	}
	if descends, err := steps.Descends(context.Background(), second, first); err != nil || descends {
		t.Fatalf("first from second: %v, %v", descends, err)
	}
	if order, err := steps.Order(context.Background(), second); err != nil || order != "workers after fleet" {
		t.Fatalf("second's order %q, %v", order, err)
	}
	if order, err := steps.Order(context.Background(), first); err != nil || order != "" {
		t.Fatalf("first's order %q, %v", order, err)
	}
	os.MkdirAll(filepath.Join(config.Out, "manifests"), 0o755)
	os.WriteFile(filepath.Join(config.Out, "current.txt"), []byte("canary Cloud\n"+manifestOf(commit("a"))+manifestOf(commit("b"))), 0o644)
	os.WriteFile(filepath.Join(config.Out, "manifests", commit("b")+".txt"), []byte(manifestOf(commit("b"))), 0o644)
	if err := steps.Promote(commit("b")); err != nil {
		t.Fatal(err)
	}
	if content, _ := os.ReadFile(filepath.Join(config.Out, "current.txt")); string(content) != manifestOf(commit("b")) {
		t.Fatalf("current.txt:\n%s", content)
	}
	if err := steps.Promote(commit("c")); err == nil {
		t.Fatal("promoted a commit with no manifest")
	}
	os.WriteFile(filepath.Join(config.Out, "manifests", commit("d")+".txt"), []byte(manifestOf(commit("e"))), 0o644)
	if err := steps.Promote(commit("d")); err == nil {
		t.Fatal("promoted a manifest naming another commit")
	}
}

// install does nothing where release.conf doesn't exist; where it does, it writes the hook, the probe and the unit,
// starts the watcher, and restarts it only when its unit or its binary changed. The hook passes on a release from
// before `loom release`.
func TestInstallOnlyWhereConfigured(t *testing.T) {
	home := t.TempDir()
	paths := HomeInstallPaths(home)
	paths.Proc = filepath.Join(home, "proc")
	var calls [][]string
	mainPid := "0"
	systemctl := func(arguments ...string) (string, error) {
		calls = append(calls, arguments)
		if arguments[0] == "show" {
			return mainPid + "\n", nil
		}
		return "", nil
	}
	var out bytes.Buffer
	if err := Install(paths, systemctl, &out); err != nil || len(calls) != 0 || !strings.Contains(out.String(), "doesn't release") {
		t.Fatalf("unconfigured: %v, %q, %s", err, calls, out.String())
	}
	for _, path := range []string{paths.Hook, paths.Probe, filepath.Join(paths.Units, UnitName)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unconfigured, %s was written", path)
		}
	}
	os.MkdirAll(filepath.Dir(paths.Config), 0o755)
	os.WriteFile(paths.Config, []byte("units = loom-plan.service loom-release.service\n"), 0o644)
	os.MkdirAll(filepath.Dir(paths.Binary), 0o755)
	os.WriteFile(paths.Binary, []byte("release one"), 0o755)
	if err := Install(paths, systemctl, &out); err != nil || !reflect.DeepEqual(calls, [][]string{{"daemon-reload"}, {"enable", UnitName}, {"show", "--property=MainPID", "--value", UnitName}, {"start", UnitName}}) {
		t.Fatalf("first install: %v, %q", err, calls)
	}
	if probe, _ := os.ReadFile(paths.Probe); !strings.Contains(string(probe), `units="loom-plan.service loom-release.service"`) {
		t.Fatalf("the probe:\n%s", probe)
	}
	mainPid = "4242"
	exe := filepath.Join(paths.Proc, "4242", "exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.Symlink(paths.Binary, exe)
	calls = nil
	if err := Install(paths, systemctl, &out); err != nil || len(calls) != 2 {
		t.Fatalf("nothing changed: %v, %q", err, calls)
	}
	os.Remove(exe)
	os.Symlink(filepath.Join(home, "old-loom"), exe)
	os.WriteFile(filepath.Join(home, "old-loom"), []byte("release zero"), 0o755)
	calls = nil
	if err := Install(paths, systemctl, &out); err != nil || !reflect.DeepEqual(calls[len(calls)-1], []string{"restart", UnitName}) {
		t.Fatalf("a new binary: %v, %q", err, calls)
	}
	run := func(loom string) (int, string) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "loom"), []byte(loom), 0o755)
		command := exec.Command(paths.Hook)
		command.Env = []string{"PATH=/usr/bin:/bin", "LOOM_UPDATE_BIN=" + bin}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	current := "#!/bin/sh\nif [ $# = 1 ]; then printf 'usage:\\n  loom release watch\\n  loom release install [--config <file>]\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran release install") {
		t.Fatalf("with release install: exit %d, %s", code, output)
	}
	rollback := "#!/bin/sh\nprintf 'usage:\\n  loom run\\n' >&2; exit 3\n"
	if code, output := run(rollback); code != 0 || !strings.Contains(output, "has no release install") {
		t.Fatalf("on a rollback: exit %d, %s", code, output)
	}
}
