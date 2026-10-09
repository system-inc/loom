package ownerbridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const change = "chg_0123456789abcdefghjkmnpqrs"

// pipeline serves the owners' feed after ?after= and one change's record, and refuses any other token.
func pipeline(t *testing.T, lines []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer coordinator" {
			http.Error(writer, "no", http.StatusUnauthorized)
			return
		}
		switch request.URL.Path {
		case "/changes/events":
			after, _ := strconv.ParseInt(request.URL.Query().Get("after"), 10, 64)
			for index, line := range lines {
				if int64(index+1) > after {
					fmt.Fprintln(writer, line)
				}
			}
		case "/changes/" + change:
			fmt.Fprintf(writer, `{"record":{"change":%q,"sha":"%s","base":"%s","owner":"system_adamic_loom_web","paths":["a"]},"state":"landed"}`, change, strings.Repeat("a", 40), strings.Repeat("b", 40))
		default:
			http.NotFound(writer, request)
		}
	}))
}

func event(seq int, kind string, data string) string {
	return fmt.Sprintf(`{"seq":%d,"at":"2026-10-10T00:00:00Z","prev":"%s","type":%q,"subject":{"change":%q,"unitKey":"%s"},"data":%s}`,
		seq, strings.Repeat("0", 64), kind, change, strings.Repeat("c", 64), data)
}

type sent struct{ owner, text string }

func bridgeFor(server *httptest.Server, state string, inbox *[]sent, failAt int) *Bridge {
	return &Bridge{
		Pipeline:  server.URL,
		Token:     func() string { return "coordinator" },
		Client:    server.Client(),
		StatePath: state,
		Send: func(owner string, text string) error {
			if failAt > 0 && len(*inbox)+1 == failAt {
				return errors.New("the inbox is down")
			}
			*inbox = append(*inbox, sent{owner, text})
			return nil
		},
	}
}

func TestSendsEachOwnerEventOnceAcrossPasses(t *testing.T) {
	lines := []string{
		event(1, "change.parked", `{"by":"chg_zzzzzzzzzzzzzzzzzzzzzzzzzz"}`),
		event(2, "change.red", `{"verdict":{"tests":[{"package":"github.com/system-inc/adamic/x","test":"TestY","outcome":"fail"},{"package":"p","test":"TestZ","outcome":"pass"}]},"extra":1}`),
	}
	server := pipeline(t, lines)
	defer server.Close()
	state := filepath.Join(t.TempDir(), "owner-bridge.seq")
	var inbox []sent
	if count, err := bridgeFor(server, state, &inbox, 0).Once(context.Background()); err != nil || count != 2 {
		t.Fatalf("first pass: %d %v", count, err)
	}
	// The next pass, from a fresh bridge as after a restart, sends only what came since.
	lines = append(lines, event(3, "change.landed", `{"main":"`+strings.Repeat("d", 40)+`","from":"`+strings.Repeat("e", 40)+`"}`))
	server.Config.Handler = pipeline(t, lines).Config.Handler
	if count, err := bridgeFor(server, state, &inbox, 0).Once(context.Background()); err != nil || count != 1 {
		t.Fatalf("second pass: %d %v", count, err)
	}
	if len(inbox) != 3 || inbox[0].owner != "system_adamic_loom_web" {
		t.Fatalf("inbox %+v", inbox)
	}
	for index, want := range []string{
		"Loom: your change " + change + " (aaaaaaaaaaaa) is parked: chg_zzzzzzzzzzzzzzzzzzzzzzzzzz was kicked below it, and yours restacks when that one moves.",
		"Loom: your change " + change + " (aaaaaaaaaaaa) is red. Failing: github.com/system-inc/adamic/x TestY. The diff: git diff bbbbbbbbbbbb..aaaaaaaaaaaa. Reproduce it: loom repro " + strings.Repeat("c", 64) + ".\n{\"extra\":1}",
		"Loom: your change " + change + " (aaaaaaaaaaaa) landed on main as dddddddddddd, on top of eeeeeeeeeeee.",
	} {
		if inbox[index].text != want {
			t.Fatalf("message %d:\n%s\nwant\n%s", index, inbox[index].text, want)
		}
	}
}

func TestAFailedSendIsSentAgainNextPassAndNothingAfterItIsSkipped(t *testing.T) {
	lines := []string{event(1, "change.landed", `{}`), event(2, "change.landed", `{}`), event(3, "change.landed", `{}`)}
	server := pipeline(t, lines)
	defer server.Close()
	state := filepath.Join(t.TempDir(), "owner-bridge.seq")
	var inbox []sent
	if count, err := bridgeFor(server, state, &inbox, 2).Once(context.Background()); err == nil || count != 1 {
		t.Fatalf("a pass whose second send fails sends one and says so: %d %v", count, err)
	}
	if count, err := bridgeFor(server, state, &inbox, 0).Once(context.Background()); err != nil || count != 2 {
		t.Fatalf("the next pass sends the rest: %d %v", count, err)
	}
	if len(inbox) != 3 {
		t.Fatalf("each event exactly once: %+v", inbox)
	}
}

func TestRefusesAFeedItCannotRead(t *testing.T) {
	server := pipeline(t, nil)
	defer server.Close()
	var inbox []sent
	bridge := bridgeFor(server, filepath.Join(t.TempDir(), "s"), &inbox, 0)
	bridge.Token = func() string { return "submit" }
	if _, err := bridge.Once(context.Background()); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a feed refused is an error: %v", err)
	}
}
