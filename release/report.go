package release

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/protocol"
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

// The headers a signed report carries: the claims of the box's report token (the token's public half), and the
// HMAC-SHA256 of the request body keyed by the whole token, in hex.
const (
	ClaimsHeader    = "X-Loom-Report-Claims"
	SignatureHeader = "X-Loom-Report-Signature"
)

// ReportRun is the run claim of a host's report token.
func ReportRun(host string) string { return "report-" + strings.ToLower(host) }

// MintReportToken mints a host's report token from the token secret, as every Loom token is minted: `loom release
// report-token <host>` on Workshop, kept on the box as ~/.loom/report-token, mode 600.
func MintReportToken(secret []byte, host string, expires time.Time) (string, error) {
	if !hostPattern.MatchString(host) {
		return "", fmt.Errorf("%q isn't a host name", host)
	}
	return protocol.MintToken(secret, protocol.TokenClaims{Run: ReportRun(host), Scope: protocol.ScopeReport, Expires: expires.Unix()})
}

// SignReport is the signature header's value for a body, keyed by the whole report token.
func SignReport(token string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ReplayWindow is how far a report's own time may be from the receiver's: a report replayed later is refused, as is
// one no newer than the host's last.
const ReplayWindow = 10 * time.Minute

// A Receiver takes the boxes' reports (POST /report) and keeps each host's latest at <Directory>/<host>.json, with
// the time it arrived and where from. Anything on the house's network can reach it, so a report counts only when all
// of these hold: it names one of Hosts; it is signed by that host's report token, which the receiver rebuilds from
// the token secret and the claims it is sent (the token itself never crosses the network); its sender's address is
// one of that host's (Addresses, else the host's name resolved); and its time is within ReplayWindow of now and newer
// than the host's last report, so none is replayed. A report a box sends otherwise is refused, named, and the box's
// updater logs the refusal.
type Receiver struct {
	Directory string
	Hosts     []string
	Secret    []byte
	Addresses map[string][]string                             // by lowercase host
	Resolve   func(context.Context, string) ([]string, error) // a host's addresses when Addresses names none
	Now       func() time.Time
	Log       io.Writer // where each refusal is named, when set
	mutex     sync.Mutex
}

func (receiver *Receiver) refuse(writer http.ResponseWriter, request *http.Request, code int, format string, arguments ...any) {
	message := fmt.Sprintf(format, arguments...)
	if receiver.Log != nil && code != http.StatusNotFound {
		fmt.Fprintf(receiver.Log, "%s loom release: refused a report from %s: %s\n", receiver.Now().UTC().Format(time.RFC3339), request.RemoteAddr, message)
	}
	http.Error(writer, message, code)
}

func (receiver *Receiver) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/report" || request.Method != http.MethodPost {
		receiver.refuse(writer, request, http.StatusNotFound, "POST /report only")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, 64<<10+1))
	if err != nil || len(body) > 64<<10 {
		receiver.refuse(writer, request, http.StatusBadRequest, "a report is one JSON object under 64 KiB")
		return
	}
	var report Report
	if err := json.Unmarshal(body, &report); err != nil {
		receiver.refuse(writer, request, http.StatusBadRequest, "a report is one JSON object: %v", err)
		return
	}
	if !hostPattern.MatchString(report.Host) {
		receiver.refuse(writer, request, http.StatusBadRequest, "host %q isn't a plain host name", report.Host)
		return
	}
	known := false
	for _, host := range receiver.Hosts {
		known = known || strings.EqualFold(host, report.Host)
	}
	if !known {
		receiver.refuse(writer, request, http.StatusForbidden, "host %q isn't one of this house's boxes (%s)", report.Host, strings.Join(receiver.Hosts, ", "))
		return
	}
	now := receiver.Now().UTC()
	// The token is the claims sent and the signature only the secret makes; the report is signed with the whole.
	claims := request.Header.Get(ClaimsHeader)
	mac := hmac.New(sha256.New, receiver.Secret)
	mac.Write([]byte(claims))
	token := claims + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	verified, err := protocol.VerifyToken(receiver.Secret, token, now)
	if claims == "" || len(receiver.Secret) == 0 || err != nil || verified.Scope != protocol.ScopeReport || verified.Run != ReportRun(report.Host) {
		receiver.refuse(writer, request, http.StatusUnauthorized, "%s's report carries no report token of its own (%v): mint one with `loom release report-token %s`", report.Host, err, report.Host)
		return
	}
	given, err := hex.DecodeString(request.Header.Get(SignatureHeader))
	want, _ := hex.DecodeString(SignReport(token, body))
	if err != nil || !hmac.Equal(given, want) {
		receiver.refuse(writer, request, http.StatusUnauthorized, "%s's report isn't signed by its report token", report.Host)
		return
	}
	from, _, _ := net.SplitHostPort(request.RemoteAddr)
	addresses := receiver.Addresses[strings.ToLower(report.Host)]
	if len(addresses) == 0 && receiver.Resolve != nil {
		resolveContext, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		addresses, _ = receiver.Resolve(resolveContext, report.Host)
		cancel()
	}
	if !slices.ContainsFunc(addresses, func(address string) bool { return sameAddress(address, from) }) {
		receiver.refuse(writer, request, http.StatusForbidden, "a report for %s came from %s, which isn't %s's address (%s)", report.Host, from, report.Host, strings.Join(addresses, ", "))
		return
	}
	at, err := time.Parse(time.RFC3339, report.At)
	if err != nil || at.Sub(now) > ReplayWindow || now.Sub(at) > ReplayWindow {
		receiver.refuse(writer, request, http.StatusForbidden, "%s's report is timed %q, not within %s of %s", report.Host, report.At, ReplayWindow, now.Format(time.RFC3339))
		return
	}
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()
	last, err := ReadReport(receiver.Directory, report.Host)
	if err != nil {
		receiver.refuse(writer, request, http.StatusInternalServerError, "%v", err)
		return
	}
	if last != nil {
		if lastAt, err := time.Parse(time.RFC3339, last.At); err == nil && !at.After(lastAt) {
			receiver.refuse(writer, request, http.StatusConflict, "%s's report is timed %s, no newer than its last (%s): a replay, or its clock went back", report.Host, report.At, last.At)
			return
		}
	}
	report.Received, report.From = now, from
	if err := writeJSON(reportPath(receiver.Directory, report.Host), report); err != nil {
		receiver.refuse(writer, request, http.StatusInternalServerError, "%v", err)
		return
	}
	writer.WriteHeader(http.StatusNoContent)
}

// sameAddress compares two IP addresses as addresses, so 10.0.0.1 and ::ffff:10.0.0.1 are one.
func sameAddress(left, right string) bool {
	leftIP, rightIP := net.ParseIP(left), net.ParseIP(right)
	return leftIP != nil && rightIP != nil && leftIP.Equal(rightIP)
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
