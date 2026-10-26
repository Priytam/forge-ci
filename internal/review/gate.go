package review

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/priytamjeepandey/forge-ci/internal/store"
)

// The merge gate: Forge's rules between a change and its merge. Evaluate is
// pure — every input is loaded from Postgres beforehand — so the same
// verdict is reached on every replica and in tests, and it never calls a
// host. A rule is "fail" when something is wrong that someone must fix
// (CI failed, changes requested, a blocking label, no ticket) and "pending"
// when the gate is waiting (approvals, CI running, a draft, a freeze).

// Eligibility says whose approvals count under a rule.
type Eligibility struct {
	Eligible  map[string]bool // email → may approve (repo role in the rule's roles, or group member)
	Group     map[string]bool // members of the rule's required group
	Bootstrap bool            // repo has no members: anyone's approval counts
}

// Verdict is the gate's outcome for one change head.
type Verdict struct {
	State     string // pass | pending | fail
	Summary   string
	Rules     []store.GateRule
	RecheckAt *time.Time // next freeze-window boundary, when the verdict may change on its own
}

// reviewer is one person across host and Forge reviews.
type reviewer struct {
	email   string // "" when a host login is not linked to a Forge user
	display string
	review  store.ChangeReview
}

// Evaluate applies policy p to the change in in at time now.
func Evaluate(in *store.GateInputs, p *store.MergePolicy, el Eligibility, now time.Time) Verdict {
	c := in.Change
	authorEmail := in.Identities[strings.ToLower(c.Author)]
	// Latest decisive verdict per person, host and Forge together.
	reviews := append([]store.ChangeReview(nil), in.Reviews...)
	sort.SliceStable(reviews, func(i, j int) bool { return reviews[i].SubmittedAt.Before(reviews[j].SubmittedAt) })
	latest := map[string]reviewer{}
	for _, r := range reviews {
		if r.Verdict == "comment" {
			continue
		}
		email, display := strings.ToLower(r.Reviewer), r.Reviewer
		if r.Source != "forge" {
			email = in.Identities[strings.ToLower(r.Reviewer)]
		}
		key := email
		if key == "" {
			key = "host:" + strings.ToLower(r.Reviewer)
		}
		latest[key] = reviewer{email: email, display: display, review: r}
	}
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var approved, fromGroup int
	var wanted, notes []string
	for _, k := range keys {
		rv := latest[k]
		r := rv.review
		self := (authorEmail != "" && rv.email == authorEmail) ||
			(r.Source != "forge" && strings.EqualFold(r.Reviewer, c.Author))
		switch r.Verdict {
		case "request_changes":
			wanted = append(wanted, rv.display)
		case "approve":
			switch {
			case self && !p.AllowSelfApproval:
				notes = append(notes, rv.display+" (own change)")
			case p.DismissOnPush && r.CommitSHA != "" && r.CommitSHA != c.HeadSHA:
				notes = append(notes, rv.display+" (earlier revision)")
			case rv.email == "" && !el.Bootstrap:
				notes = append(notes, rv.display+" (not linked to a Forge user)")
			case rv.email != "" && !el.Bootstrap && !el.Eligible[rv.email]:
				notes = append(notes, rv.display+" (no approver role)")
			default:
				approved++
				if rv.email != "" && el.Group[rv.email] {
					fromGroup++
				}
			}
		}
	}

	var rules []store.GateRule
	add := func(id, label, state, detail string) {
		rules = append(rules, store.GateRule{ID: id, Label: label, State: state, Detail: detail})
	}
	if c.Draft {
		add("draft", "Ready for review", "pending", "The change is a draft")
	}
	if p.RequiredApprovals > 0 {
		state := "pending"
		if approved >= p.RequiredApprovals {
			state = "pass"
		}
		detail := fmt.Sprintf("%d of %d approvals", approved, p.RequiredApprovals)
		if len(notes) > 0 {
			detail += "; not counted: " + strings.Join(notes, ", ")
		}
		add("approvals", "Approvals", state, detail)
	}
	if p.RequiredGroup != "" && p.RequiredGroupApprovals > 0 {
		state := "pending"
		if fromGroup >= p.RequiredGroupApprovals {
			state = "pass"
		}
		add("group", "Approval from "+p.RequiredGroup, state,
			fmt.Sprintf("%d of %d from %s", fromGroup, p.RequiredGroupApprovals, p.RequiredGroup))
	}
	if p.BlockOnChangesRequested {
		if len(wanted) > 0 {
			add("changes_requested", "No changes requested", "fail", "Changes requested by "+strings.Join(wanted, ", "))
		} else {
			add("changes_requested", "No changes requested", "pass", "Nobody has requested changes")
		}
	}
	if p.RequireCI || len(p.RequiredJobs) > 0 {
		state, detail := ciRule(in, p)
		add("ci", "CI", state, detail)
	}
	if len(p.RequiredLabels) > 0 || len(p.BlockedLabels) > 0 {
		state, detail := labelRule(in.Labels, p)
		add("labels", "Labels", state, detail)
	}
	if p.TicketPattern != "" {
		re := regexp.MustCompile(p.TicketPattern) // validated when the rule was saved
		if re.MatchString(in.Title + "\n" + in.Body + "\n" + in.Source) {
			add("ticket", "Linked ticket", "pass", "Ticket: "+re.FindString(in.Title+"\n"+in.Body+"\n"+in.Source))
		} else {
			add("ticket", "Linked ticket", "fail", "No ticket matching "+p.TicketPattern+" in the title, description or branch")
		}
	}
	var recheck *time.Time
	if len(p.FreezeWindows) > 0 {
		frozen, until, next := freeze(p, now)
		recheck = &next
		if frozen {
			add("freeze", "Merge window", "pending", "Merge freeze until "+until)
		} else {
			add("freeze", "Merge window", "pass", "Outside the freeze windows")
		}
	}

	v := Verdict{State: "pass", Rules: rules, RecheckAt: recheck}
	for _, want := range []string{"fail", "pending"} {
		for _, r := range rules {
			if r.State == want {
				v.State, v.Summary = want, r.Detail
				return v
			}
		}
	}
	v.Summary = fmt.Sprintf("All %d merge rules pass", len(rules))
	if len(rules) == 0 {
		v.Summary = "No merge rules to meet"
	}
	return v
}

func ciRule(in *store.GateInputs, p *store.MergePolicy) (string, string) {
	pl := in.Pipeline
	short := in.Change.HeadSHA
	if len(short) > 7 {
		short = short[:7]
	}
	if pl == nil {
		return "pending", "Waiting for a Forge pipeline on " + short
	}
	for _, name := range p.RequiredJobs {
		switch st, ok := pl.Jobs[name]; {
		case !ok:
			return "fail", fmt.Sprintf("Required job %q is not in pipeline #%d", name, pl.ID)
		case st == "failed" || st == "canceled":
			return "fail", fmt.Sprintf("Required job %q %s", name, st)
		case st != "success":
			return "pending", fmt.Sprintf("Waiting for required job %q", name)
		}
	}
	if !p.RequireCI {
		return "pass", "Required jobs passed"
	}
	switch pl.Status {
	case "success":
		return "pass", fmt.Sprintf("Pipeline #%d passed", pl.ID)
	case "failed", "canceled":
		return "fail", fmt.Sprintf("Pipeline #%d %s", pl.ID, pl.Status)
	case "blocked":
		return "pending", fmt.Sprintf("Pipeline #%d is waiting for an approval", pl.ID)
	}
	return "pending", fmt.Sprintf("Pipeline #%d is running", pl.ID)
}

func labelRule(labels []string, p *store.MergePolicy) (string, string) {
	have := map[string]bool{}
	for _, l := range labels {
		have[strings.ToLower(l)] = true
	}
	for _, l := range p.BlockedLabels {
		if have[strings.ToLower(l)] {
			return "fail", "Labelled " + l
		}
	}
	var missing []string
	for _, l := range p.RequiredLabels {
		if !have[strings.ToLower(l)] {
			missing = append(missing, l)
		}
	}
	if len(missing) > 0 {
		return "pending", "Needs label " + strings.Join(missing, ", ")
	}
	return "pass", "Labels in order"
}

const minutesPerWeek = 7 * 24 * 60

// freeze reports whether now falls in one of p's weekly windows, when the
// window ends (as "Mon 09:00"), and the next boundary of any window.
func freeze(p *store.MergePolicy, now time.Time) (frozen bool, until string, next time.Time) {
	loc, err := time.LoadLocation(p.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	m := (int(local.Weekday())+6)%7*24*60 + local.Hour()*60 + local.Minute()
	after := func(target int) int { // minutes from m until target, in (0, week]
		d := (target - m + minutesPerWeek) % minutesPerWeek
		if d == 0 {
			d = minutesPerWeek
		}
		return d
	}
	soonest := minutesPerWeek
	for _, w := range p.FreezeWindows {
		a, err1 := store.ParseWeekTime(w.Start)
		b, err2 := store.ParseWeekTime(w.End)
		if err1 != nil || err2 != nil {
			continue
		}
		in := (a < b && m >= a && m < b) || (a > b && (m >= a || m < b))
		if in && !frozen {
			frozen = true
			until = local.Add(time.Duration(after(b)) * time.Minute).Truncate(time.Minute).Format("Mon 15:04 MST")
		}
		soonest = min(soonest, after(a), after(b))
	}
	start := local.Truncate(time.Minute)
	return frozen, until, start.Add(time.Duration(soonest) * time.Minute)
}
