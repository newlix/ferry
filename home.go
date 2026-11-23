package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/at-least/ciderui/go/console"
	"github.com/at-least/ciderui/go/lucide"
	g "maragu.dev/gomponents"
	h "maragu.dev/gomponents/html"
)

// homeRefreshSeconds reloads the open page; the data behind it changes only
// every usageRefreshInterval, so faster reloads would show nothing new.
const homeRefreshSeconds = 60

// planEndingSoon marks a plan end date worth noticing.
const planEndingSoon = 30 * 24 * time.Hour

func (gw *Gateway) handleHome(w http.ResponseWriter, r *http.Request) {
	usage, known, refreshedAt := gw.usage.snapshot(len(gw.cfg.APIKeys))
	page := homePage(homeData{
		keys:        gw.cfg.APIKeys,
		usage:       usage,
		known:       known,
		refreshedAt: refreshedAt,
		baseURL:     requestBaseURL(r) + "/v1",
		models:      gw.cfg.ModelNames(),
		route:       gw.cfg.Route,
		now:         time.Now(),
	})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := page.Render(w); err != nil {
		gw.log.Quiet.Printf("GET /: render: %v", err)
	}
}

// requestBaseURL is the address the browser used, so the page shows the
// base URL that works from where the reader is (localhost or
// a hostname behind a reverse proxy that sets X-Forwarded-Proto).
func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

type homeData struct {
	keys        []apiKey
	usage       []keyUsage
	known       []bool
	refreshedAt time.Time
	baseURL     string
	models      []string
	route       string
	now         time.Time
}

func homePage(d homeData) g.Node {
	var body g.Node = console.ConsoleEmpty{Size: console.ConsoleEmptySizeLarge, Icon: lucide.KeyRound,
		Title: "No keys yet", Body: "Add one with ferry key add."}
	if len(d.keys) > 0 {
		rows := make([]console.ConsoleTableRow, len(d.keys))
		for i, k := range d.keys {
			rows[i] = keyRow(k.Name, d.usage[i], d.known[i], d.refreshedAt.IsZero(), d.now)
		}
		body = console.ConsoleTable{
			Columns: []console.ConsoleTableColumn{{Label: "Key"}, {Label: "This week"}, {Label: "5-hour pool"}, {Label: "Plan"}},
			Rows:    rows,
		}
	}
	return console.ConsolePage{
		Lang:  "en",
		Title: "ferry",
		Head:  h.Meta(g.Attr("http-equiv", "refresh"), h.Content(fmt.Sprint(homeRefreshSeconds))),
		Children: console.ConsoleContent{
			Title:  "ferry",
			Detail: summaryLine(d),
			Children: g.Group{
				console.ConsoleSection{Title: "Keys", Children: body},
				console.ConsoleSection{Title: "Connect", Children: h.Dl(h.Class("cui-console-dl"),
					h.Dt(g.Text("Base URL")), h.Dd(g.Text(d.baseURL)),
					h.Dt(g.Text("API key")), h.Dd(g.Text("Any value. ferry adds the real key.")),
					h.Dt(g.Text("Models")), h.Dd(g.Text(strings.Join(d.models, " · "))),
				)},
			},
		},
	}
}

func summaryLine(d homeData) string {
	keys := fmt.Sprintf("%d keys", len(d.keys))
	if len(d.keys) == 1 {
		keys = "1 key"
	}
	if d.route != "" {
		keys += " · fixed to " + d.route
	}
	if d.refreshedAt.IsZero() {
		return "Running · " + keys + " · checking quota…"
	}
	return "Running · " + keys + " · quota checked " + ago(d.now.Sub(d.refreshedAt))
}

// keyRow: an unknown key's quota and plan cells are nil, which the table
// writes as "—".
func keyRow(name string, u keyUsage, known, checking bool, now time.Time) console.ConsoleTableRow {
	if !known {
		msg := "Quota unknown. z.ai did not answer at the last check."
		if checking {
			msg = "Checking quota…"
		}
		return console.ConsoleTableRow{Cells: []g.Node{withSubtitle(name, msg), nil, nil, nil}}
	}
	weeklyNote := ""
	if !u.weeklyReset.IsZero() {
		weeklyNote = "Resets in " + relTime(u.weeklyReset.Sub(now)) + " · " + u.weeklyReset.Format("Mon 15:04")
	}
	sessionNote := "Full"
	if !u.sessionReset.IsZero() {
		sessionNote = "Refills in " + relTime(u.sessionReset.Sub(now))
	}
	return console.ConsoleTableRow{Cells: []g.Node{
		withSubtitle(name, u.planName),
		quotaStatus(u.weeklyRem, weeklyNote),
		quotaStatus(u.sessionRem, sessionNote),
		planCell(u, now),
	}}
}

func withSubtitle(text, subtitle string) g.Node {
	return g.Group{g.Text(text), g.If(subtitle != "", h.Span(h.Class("cui-console-table-detail"), g.Text(subtitle)))}
}

func quotaStatus(remaining float64, note string) g.Node {
	pct := int(math.Round(math.Max(0, math.Min(1, remaining)) * 100))
	color := console.ConsoleColorSuccess
	switch {
	case remaining < sessionLowThreshold:
		color = console.ConsoleColorError
	case remaining < 0.5:
		color = console.ConsoleColorWarning
	}
	return console.ConsoleStatus{Label: fmt.Sprintf("%d%% left", pct), Color: color, Detail: note}
}

func planCell(u keyUsage, now time.Time) g.Node {
	if u.planEnd.IsZero() {
		return nil
	}
	verb := "Ends"
	if u.autoRenew {
		verb = "Renews"
	}
	left := u.planEnd.Sub(now)
	label := verb + " " + u.planEnd.Format("Jan 2, 2006")
	detail := fmt.Sprintf("in %d days", int(math.Ceil(left.Hours()/24)))
	if !u.autoRenew && left < planEndingSoon {
		return console.ConsoleStatus{Label: label, Color: console.ConsoleColorWarning, Detail: detail}
	}
	return withSubtitle(label, detail)
}

// ago renders how long ago something happened: "just now", "3 min ago".
func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return relTime(d) + " ago"
}
