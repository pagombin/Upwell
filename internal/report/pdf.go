package report

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"

	"github.com/pagombin/upwell/internal/orch"
)

// PDF renders the report as PDF locally (overnight item 8): the same sections
// as the HTML report, drawn with the vendored go-pdf/fpdf (third_party/fpdf)
// and its built-in core fonts. Nothing is fetched and no browser is used.
func (r *Report) PDF() ([]byte, error) {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetTitle("Upwell report: "+r.Migration.Name, true)
	p.SetCreator("Upwell "+r.AppVersion, true)
	p.SetMargins(15, 15, 15)
	p.SetAutoPageBreak(true, 15)
	tr := p.UnicodeTranslatorFromDescriptor("") // cp1252 for the core fonts
	txt := func(s string) string {
		return tr(strings.NewReplacer("→", "->", "…", "...", "—", "-", "·", "-").Replace(s))
	}
	p.SetFooterFunc(func() {
		p.SetY(-12)
		p.SetFont("Helvetica", "", 8)
		p.SetTextColor(110, 120, 130)
		p.CellFormat(0, 5, txt(fmt.Sprintf("Upwell report - %s - customer-confidential - page %d", r.Migration.Name, p.PageNo())), "", 0, "C", false, 0, "")
	})
	p.AddPage()
	width, _ := p.GetPageSize()
	usable := width - 30

	h1 := func(s string) {
		p.SetFont("Helvetica", "B", 18)
		p.SetTextColor(20, 33, 43)
		p.MultiCell(0, 8, txt(s), "", "L", false)
	}
	h2 := func(s string) {
		p.Ln(4)
		p.SetFont("Helvetica", "B", 13)
		p.SetTextColor(20, 33, 43)
		p.CellFormat(0, 7, txt(s), "B", 1, "L", false, 0, "")
		p.Ln(1)
	}
	para := func(s string) {
		p.SetFont("Helvetica", "", 9.5)
		p.SetTextColor(20, 33, 43)
		p.MultiCell(0, 4.8, txt(s), "", "L", false)
	}
	muted := func(s string) {
		p.SetFont("Helvetica", "", 8.5)
		p.SetTextColor(90, 104, 116)
		p.MultiCell(0, 4.2, txt(s), "", "L", false)
	}
	// table draws rows whose cells wrap; widths are fractions of the page.
	table := func(head []string, frac []float64, rows [][]string, color func(row []string) (int, int, int)) {
		ws := make([]float64, len(frac))
		for i, f := range frac {
			ws[i] = f * usable
		}
		line := 4.2
		p.SetFont("Helvetica", "B", 8)
		p.SetTextColor(90, 104, 116)
		for i, hd := range head {
			p.CellFormat(ws[i], 6, txt(hd), "B", 0, "L", false, 0, "")
		}
		p.Ln(-1)
		p.SetFont("Helvetica", "", 8.5)
		for _, row := range rows {
			n := 1
			for i, c := range row {
				if k := len(p.SplitLines([]byte(txt(c)), ws[i]-1.5)); k > n {
					n = k
				}
			}
			h := float64(n)*line + 1.5
			if p.GetY()+h > 282 {
				p.AddPage()
			}
			x, y := p.GetXY()
			for i, c := range row {
				p.SetXY(x, y)
				if color != nil && i == len(row)-1 {
					cr, cg, cb := color(row)
					p.SetTextColor(cr, cg, cb)
				} else {
					p.SetTextColor(20, 33, 43)
				}
				p.MultiCell(ws[i], line, txt(c), "", "L", false)
				x += ws[i]
			}
			p.SetXY(15, y+h)
			p.SetDrawColor(219, 226, 231)
			p.Line(15, y+h-0.5, 15+usable, y+h-0.5)
		}
	}

	muted("Upwell migration report - generated " + ts(r.GeneratedAt) + " - customer-confidential: contains database and table names and sizes")
	h1(r.Migration.Name)
	v := r.Migration.Flags.Verdict
	if v == "" {
		v = "no verdict yet"
	}
	p.SetFont("Helvetica", "B", 12)
	switch v {
	case "GO":
		p.SetTextColor(23, 101, 58)
	case "NO-GO":
		p.SetTextColor(161, 38, 29)
	default:
		p.SetTextColor(90, 104, 116)
	}
	p.CellFormat(0, 7, txt("Verdict: "+v), "", 1, "L", false, 0, "")
	para(fmt.Sprintf("Migration %s - state %s - engine pgcopydb %s - Upwell %s", r.Migration.ShortID, orch.Label(r.Migration.State), r.EngineVersion, r.AppVersion))
	if c := r.Migration.Flags.Cutover; c != nil && c.WritePauseMS > 0 {
		para("Write pause: " + dur(c.WritePauseMS))
	}

	h2("Customer permission")
	if pm := r.Permission; pm != nil {
		para(fmt.Sprintf("Customer %s (account %s), ticket %s, granted by %s on %s. Scope: %s", pm.Customer, pm.AccountID, pm.Ticket, pm.GrantedBy, pm.GrantedAt, pm.Scope))
	} else {
		para("No permission record.")
	}
	h2("Clusters")
	var cl [][]string
	if c := r.Source; c != nil {
		cl = append(cl, []string{"Source", c.Host, fmt.Sprint(c.Port), c.User, c.SSLMode})
	}
	if c := r.Target; c != nil {
		cl = append(cl, []string{"Target", c.Host, fmt.Sprint(c.Port), c.User, c.SSLMode})
	}
	table([]string{"Cluster", "Host", "Port", "User", "SSL"}, []float64{.14, .46, .1, .16, .14}, cl, nil)

	h2("Databases")
	var dr [][]string
	for _, d := range r.Databases {
		if d.Include {
			dr = append(dr, []string{d.SourceName, d.TargetName, bytesH(d.SizeBytes), orch.Label(d.State), d.Plugin, orDash(d.Verdict)})
		}
	}
	verdictColor := func(row []string) (int, int, int) {
		switch row[len(row)-1] {
		case "GO", "OK", "ok":
			return 23, 101, 58
		case "NO-GO", "BLOCKER", "blocker":
			return 161, 38, 29
		case "WARNING", "warning":
			return 138, 83, 0
		}
		return 20, 33, 43
	}
	table([]string{"Source", "Target", "Size", "State", "Plugin", "Verdict"}, []float64{.22, .22, .12, .16, .14, .14}, dr, verdictColor)

	h2("After cutover")
	if len(r.Checklist) == 0 {
		para("Nothing left to do.")
	}
	for i, it := range r.Checklist {
		p.SetFont("Helvetica", "B", 9.5)
		p.SetTextColor(20, 33, 43)
		dbs := ""
		if len(it.Databases) > 0 {
			dbs = " (" + strings.Join(it.Databases, ", ") + ")"
		}
		p.MultiCell(0, 5, txt(fmt.Sprintf("%d. %s%s", i+1, it.Title, dbs)), "", "L", false)
		para(it.Detail)
		if it.SQL != "" {
			p.SetFont("Courier", "", 7.5)
			p.SetFillColor(244, 246, 248)
			p.MultiCell(0, 3.6, txt(strings.TrimRight(it.SQL, "\n")), "", "L", true)
		}
		p.Ln(1)
	}

	h2("Verification")
	if len(r.Verification) == 0 {
		para("Not verified yet.")
	} else {
		var vr [][]string
		for _, x := range r.Verification {
			vr = append(vr, []string{x.Database, x.Title, detailMsg(x.Detail), strings.ToUpper(x.Result)})
		}
		table([]string{"Database", "Check", "Detail", "Result"}, []float64{.18, .2, .5, .12}, vr, verdictColor)
	}

	h2("Accepted risks")
	if len(r.Accepted) == 0 {
		para("None.")
	} else {
		var ar [][]string
		for _, a := range r.Accepted {
			ar = append(ar, []string{fmt.Sprint(a["check_id"]), strings.TrimSpace(fmt.Sprint(a["scope"], " ", a["database"])), fmt.Sprint(a["reason"]), fmt.Sprint(a["accepted_by"]), ts(a["accepted_at"])})
		}
		table([]string{"Check", "Scope", "Reason", "By", "When"}, []float64{.18, .16, .38, .12, .16}, ar, nil)
	}

	h2("Preflight")
	counts := map[string]int{}
	var pr [][]string
	for _, x := range r.Preflight {
		counts[x.Level]++
		if x.Level == "blocker" || x.Level == "warning" {
			lvl := x.Level
			if x.Hard {
				lvl += " (hard)"
			}
			if x.Accepted != nil {
				lvl += " (accepted)"
			}
			pr = append(pr, []string{x.Title, strings.TrimSpace(x.Scope + " " + x.Database), x.Message, lvl})
		}
	}
	muted(fmt.Sprintf("%d checks: %d ok, %d info, %d warnings, %d blockers. Blockers and warnings:", len(r.Preflight), counts["ok"], counts["info"], counts["warning"], counts["blocker"]))
	if len(pr) > 0 {
		table([]string{"Check", "Scope", "Message", "Level"}, []float64{.2, .14, .52, .14}, pr, func(row []string) (int, int, int) {
			if strings.HasPrefix(row[3], "blocker") {
				return 161, 38, 29
			}
			return 138, 83, 0
		})
	}

	h2("Engine attempts")
	var at [][]string
	for _, a := range r.Attempts {
		code := "-"
		if a.ExitCode != nil {
			code = fmt.Sprint(*a.ExitCode)
		}
		at = append(at, []string{a.Database, fmt.Sprint(a.Number), a.Kind, ts(a.StartedAt), ts(a.EndedAt), code, a.Reason})
	}
	table([]string{"Database", "#", "Kind", "Started", "Ended", "Exit", "Reason"}, []float64{.17, .05, .09, .19, .19, .06, .25}, at, nil)

	h2("Timeline")
	var tl [][]string
	for _, e := range r.Events {
		tl = append(tl, []string{ts(e["ts"]), fmt.Sprint(e["database"]), fmt.Sprint(e["message"])})
	}
	table([]string{"Time", "Database", "Event"}, []float64{.2, .16, .64}, tl, nil)

	var b bytes.Buffer
	if err := p.Output(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
