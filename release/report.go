package release

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Report is what a box's updater posts (updater/loom-update.sh's post), whenever its state changes and every 5
// minutes besides: the version it runs, the one before, when it last switched, the version whose hooks all passed, its
// hold, its last run's refusal, and a line per service its health probes watch. Received and From are the receiver's.
type Report struct {
	Host     string    `json:"host"`
	Version  string    `json:"version"`
	Previous string    `json:"previous"`
	Updated  string    `json:"updated"`
	Hooked   string    `json:"hooked"`
	Held     string    `json:"held"`
	Refused  string    `json:"refused"`
	Services []string  `json:"services"`
	At       string    `json:"at"`
	Received time.Time `json:"received"`
	From     string    `json:"from"`
}

// A Service is one health probe line, "<service> <state> [<substate>] [<key>=<value>...]": a systemd unit's
// ActiveState and SubState, and its NRestarts as restarts (-1 when the line doesn't say).
type Service struct {
	Name     string
	State    string
	Sub      string
	Restarts int
}

// ParseService reads one probe line; a line with fewer than two fields is a service with no state.
func ParseService(line string) Service {
	fields := strings.Fields(line)
	service := Service{Restarts: -1}
	if len(fields) > 0 {
		service.Name = fields[0]
	}
	if len(fields) > 1 {
		service.State = fields[1]
	}
	for _, field := range fields[min(len(fields), 2):] {
		key, value, found := strings.Cut(field, "=")
		switch {
		case !found && service.Sub == "":
			service.Sub = field
		case key == "restarts":
			if count, err := strconv.Atoi(value); err == nil {
				service.Restarts = count
			}
		}
	}
	return service
}

// Service is the report's line for the named service, if it has one.
func (report Report) Service(name string) (Service, bool) {
	for _, line := range report.Services {
		if service := ParseService(line); service.Name == name {
			return service, true
		}
	}
	return Service{}, false
}

// Unhealthy names every service the report carries that isn't active, and every one of wanted it doesn't carry at
// all; nothing when they are all active.
func (report Report) Unhealthy(wanted []string) []string {
	var problems []string
	for _, name := range wanted {
		if _, found := report.Service(name); !found {
			problems = append(problems, name+" not reported")
		}
	}
	for _, line := range report.Services {
		if service := ParseService(line); service.State != "active" {
			problems = append(problems, strings.TrimSpace(service.Name+" "+service.State+" "+service.Sub))
		}
	}
	return problems
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// reportPath is where a host's latest report is kept: its name in lowercase, since the canary line and the boxes
// compare names ignoring case.
func reportPath(directory, host string) string {
	return filepath.Join(directory, strings.ToLower(host)+".json")
}

// A Receiver takes the boxes' reports (POST /report) and keeps each host's latest at <Directory>/<host>.json, with
// the time it arrived and where from. Only the hosts it is given are taken; a report naming any other is refused, so
// a typo in a box's host setting is seen at once rather than read as a box that never reports.
type Receiver struct {
	Directory string
	Hosts     []string
	Now       func() time.Time
}

func (receiver Receiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/report" || request.Method != http.MethodPost {
		http.Error(writer, "POST /report only", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		http.Error(writer, "a report is one JSON object under 64 KiB", http.StatusBadRequest)
		return
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		http.Error(writer, "a report is one JSON object: "+err.Error(), http.StatusBadRequest)
		return
	}
	if !hostPattern.MatchString(report.Host) {
		http.Error(writer, fmt.Sprintf("host %q isn't a plain host name", report.Host), http.StatusBadRequest)
		return
	}
	known := false
	for _, host := range receiver.Hosts {
		known = known || strings.EqualFold(host, report.Host)
	}
	if !known {
		http.Error(writer, fmt.Sprintf("host %q isn't one of this house's boxes (%s)", report.Host, strings.Join(receiver.Hosts, ", ")), http.StatusForbidden)
		return
	}
	report.Received = receiver.Now().UTC()
	report.From, _, _ = net.SplitHostPort(request.RemoteAddr)
	if err := writeJSON(reportPath(receiver.Directory, report.Host), report); err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// ReadReport is a host's latest report, or nil when it has never reported.
func ReadReport(directory, host string) (*Report, error) {
	content, err := os.ReadFile(reportPath(directory, host))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var report Report
	if err := json.Unmarshal(content, &report); err != nil {
		return nil, fmt.Errorf("%s: %w", reportPath(directory, host), err)
	}
	return &report, nil
}

// writeJSON writes value beside path and renames it over, so a reader never sees half of it.
func writeJSON(path string, value any) error {
	content, err := json.MarshalIndent(value, "", "\t")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	partial, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = partial.Write(append(content, '\n'))
	if closeErr := partial.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(partial.Name(), 0o644)
	}
	if err == nil {
		err = os.Rename(partial.Name(), path)
	}
	if err != nil {
		os.Remove(partial.Name())
	}
	return err
}
