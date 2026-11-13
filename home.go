package main

import (
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	g "maragu.dev/gomponents"
	c "maragu.dev/gomponents/components"
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
	now         time.Time
}

func homePage(d homeData) g.Node {
	return c.HTML5(c.HTML5Props{
		Title:    "ferry",
		Language: "en",
		Head: g.Group{
			h.Meta(g.Attr("http-equiv", "refresh"), h.Content(fmt.Sprint(homeRefreshSeconds))),
			h.Meta(h.Name("color-scheme"), h.Content("light dark")),
			h.StyleEl(g.Raw(homeCSS)),
		},
		Body: g.Group{h.Main(
			h.Header(
				h.H1(g.Text("ferry")),
				h.P(h.Class("sub"), g.Text(summaryLine(d))),
			),
			g.If(len(d.keys) == 0, h.P(h.Class("card empty"), g.Text("No keys yet. Add one with ferry key add."))),
			g.Map(indices(len(d.keys)), func(i int) g.Node {
				return keyCard(d.keys[i].Name, d.usage[i], d.known[i], d.refreshedAt.IsZero(), d.now)
			}),
			h.Section(h.Class("card connect"),
				h.H2(g.Text("Connect")),
				h.Dl(
					h.Dt(g.Text("Base URL")), h.Dd(h.Code(g.Text(d.baseURL))),
					h.Dt(g.Text("API key")), h.Dd(g.Text("Any value. ferry adds the real key.")),
					h.Dt(g.Text("Models")), h.Dd(g.Text(strings.Join(d.models, " · "))),
				),
			),
		)},
	})
}

func summaryLine(d homeData) string {
	keys := fmt.Sprintf("%d keys", len(d.keys))
	if len(d.keys) == 1 {
		keys = "1 key"
	}
	if d.refreshedAt.IsZero() {
		return "Running · " + keys + " · checking quota…"
	}
	return "Running · " + keys + " · quota checked " + ago(d.now.Sub(d.refreshedAt))
}

func keyCard(name string, u keyUsage, known, checking bool, now time.Time) g.Node {
	head := h.Div(h.Class("head"),
		h.H2(g.Text(name)),
		g.If(known, h.Span(h.Class("plan"), g.Text(u.planName))),
	)
	if !known {
		msg := "Quota unknown. z.ai did not answer at the last check."
		if checking {
			msg = "Checking quota…"
		}
		return h.Section(h.Class("card"), head, h.P(h.Class("muted"), g.Text(msg)))
	}

	weeklyNote := ""
	if !u.weeklyReset.IsZero() {
		weeklyNote = "Resets in " + relTime(u.weeklyReset.Sub(now)) + " · " + u.weeklyReset.Format("Mon 15:04")
	}
	sessionNote := "Full"
	if !u.sessionReset.IsZero() {
		sessionNote = "Refills in " + relTime(u.sessionReset.Sub(now))
	}
	return h.Section(h.Class("card"), head,
		meter("This week", u.weeklyRem, weeklyNote),
		meter("5-hour pool", u.sessionRem, sessionNote),
		planRow(u, now),
	)
}

func meter(label string, remaining float64, note string) g.Node {
	pct := int(math.Round(math.Max(0, math.Min(1, remaining)) * 100))
	level := "ok"
	switch {
	case remaining < sessionLowThreshold:
		level = "low"
	case remaining < 0.5:
		level = "mid"
	}
	return h.Div(h.Class("meter"),
		h.Div(h.Class("row"),
			h.Span(g.Text(label)),
			h.Span(h.Class("value"), g.Textf("%d%% left", pct)),
		),
		h.Div(h.Class("bar"), h.Div(h.Class("fill "+level), h.Style(fmt.Sprintf("width:%d%%", pct)))),
		g.If(note != "", h.P(h.Class("muted"), g.Text(note))),
	)
}

func planRow(u keyUsage, now time.Time) g.Node {
	if u.planEnd.IsZero() {
		return h.P(h.Class("plan-end muted"), g.Text("Plan end date unknown."))
	}
	verb := "Ends"
	if u.autoRenew {
		verb = "Renews"
	}
	left := u.planEnd.Sub(now)
	text := fmt.Sprintf("%s %s · in %d days", verb, u.planEnd.Format("Jan 2, 2006"), int(math.Ceil(left.Hours()/24)))
	class := "plan-end"
	if !u.autoRenew && left < planEndingSoon {
		class += " soon"
	}
	return h.P(h.Class(class), g.Text(text))
}

// ago renders how long ago something happened: "just now", "3 min ago".
func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return relTime(d) + " ago"
}

func indices(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

const homeCSS = `
:root {
  --bg: #f5f5f7; --card: #fff; --text: #1d1d1f; --muted: #6e6e73; --line: #e5e5ea;
  --ok: #34c759; --mid: #ff9f0a; --low: #ff3b30; --track: #e5e5ea;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #000; --card: #1c1c1e; --text: #f5f5f7; --muted: #98989d; --line: #38383a;
    --ok: #30d158; --mid: #ff9f0a; --low: #ff453a; --track: #38383a;
  }
}
* { box-sizing: border-box; }
body { margin: 0; background: var(--bg); color: var(--text);
  font: 15px/1.45 -apple-system, BlinkMacSystemFont, "SF Pro Text", system-ui, sans-serif; }
main { max-width: 560px; margin: 0 auto; padding: 40px 16px 64px; }
h1 { font-size: 34px; font-weight: 700; margin: 0; letter-spacing: -0.02em; }
h2 { font-size: 20px; font-weight: 600; margin: 0; }
.sub { color: var(--muted); margin: 4px 0 24px; }
.card { background: var(--card); border-radius: 16px; padding: 20px; margin: 0 0 16px; }
.head { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; margin-bottom: 16px; }
.plan { color: var(--muted); }
.meter { margin-bottom: 16px; }
.row { display: flex; justify-content: space-between; }
.value { font-variant-numeric: tabular-nums; font-weight: 600; }
.bar { height: 8px; border-radius: 4px; background: var(--track); margin: 6px 0 4px; overflow: hidden; }
.fill { height: 100%; border-radius: 4px; }
.fill.ok { background: var(--ok); } .fill.mid { background: var(--mid); } .fill.low { background: var(--low); }
.muted { color: var(--muted); margin: 0; font-size: 13px; }
.plan-end { margin: 0; padding-top: 14px; border-top: 1px solid var(--line); }
.plan-end.soon { color: var(--mid); font-weight: 600; }
.connect h2 { margin-bottom: 12px; }
dl { display: grid; grid-template-columns: max-content 1fr; gap: 8px 16px; margin: 0; }
dt { color: var(--muted); } dd { margin: 0; overflow-wrap: anywhere; }
code { font: 13px ui-monospace, "SF Mono", Menlo, monospace; }
`
