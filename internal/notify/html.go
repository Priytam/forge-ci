package notify

import (
	"fmt"
	"html"
	"strings"
)

// HTML email: a table layout with inline styles only (Gmail and Outlook
// drop <style> blocks), 560px wide, readable on a phone. The plain-text
// part is sent alongside, so text-only clients lose nothing.

type banner struct{ bg, fg, label string }

func bannerFor(kind string) banner {
	switch kind {
	case EventApprovalNeeded:
		return banner{"#b45309", "#ffffff", "Waiting for approval"}
	case EventPipelineFailed, EventDeployFailed:
		return banner{"#b91c1c", "#ffffff", "Failed"}
	case EventPipelineFixed:
		return banner{"#15803d", "#ffffff", "Fixed"}
	case EventPipelineSucceeded:
		return banner{"#15803d", "#ffffff", "Succeeded"}
	case EventDeploySucceeded:
		return banner{"#15803d", "#ffffff", "Deployed"}
	}
	return banner{"#374151", "#ffffff", "Forge"}
}

// RenderHTML renders ev for one recipient; links are that recipient's
// signed approve/reject links (zero value: no buttons).
func RenderHTML(ev Event, links ApprovalLinks) string {
	b := bannerFor(ev.Kind)
	esc := html.EscapeString
	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>`)
	sb.WriteString(esc(ev.Title))
	sb.WriteString(`</title></head><body style="margin:0;padding:0;background:#f3f4f6;font-family:-apple-system,Segoe UI,Helvetica,Arial,sans-serif;color:#111827;">`)
	sb.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="background:#f3f4f6;"><tr><td align="center" style="padding:24px 12px;">`)
	sb.WriteString(`<table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="max-width:560px;background:#ffffff;border-radius:10px;overflow:hidden;">`)
	// Banner
	fmt.Fprintf(&sb, `<tr><td style="background:%s;color:%s;padding:14px 24px;font-size:13px;font-weight:600;letter-spacing:.04em;text-transform:uppercase;">%s</td></tr>`, b.bg, b.fg, esc(b.label))
	// Title + summary
	fmt.Fprintf(&sb, `<tr><td style="padding:22px 24px 8px;font-size:18px;font-weight:700;line-height:1.3;">%s</td></tr>`, esc(ev.Title))
	summary := strings.SplitN(ev.Text, "\n", 2)[0]
	if summary != "" {
		fmt.Fprintf(&sb, `<tr><td style="padding:0 24px 16px;font-size:15px;line-height:1.5;color:#374151;">%s</td></tr>`, esc(summary))
	}
	// Details table
	fields := detailRows(ev)
	if len(fields) > 0 {
		sb.WriteString(`<tr><td style="padding:0 24px 16px;"><table role="presentation" width="100%" cellpadding="0" cellspacing="0" border="0" style="border-collapse:collapse;font-size:14px;">`)
		for _, f := range fields {
			fmt.Fprintf(&sb, `<tr><td style="padding:7px 10px 7px 0;color:#6b7280;white-space:nowrap;vertical-align:top;border-top:1px solid #e5e7eb;width:140px;">%s</td><td style="padding:7px 0;border-top:1px solid #e5e7eb;word-break:break-word;">%s</td></tr>`, esc(f[0]), esc(f[1]))
		}
		sb.WriteString(`</table></td></tr>`)
	}
	// Buttons
	sb.WriteString(`<tr><td style="padding:4px 24px 24px;"><table role="presentation" cellpadding="0" cellspacing="0" border="0"><tr>`)
	if links.Approve != "" {
		fmt.Fprintf(&sb, `<td style="padding-right:10px;"><a href="%s" style="display:inline-block;background:#15803d;color:#ffffff;text-decoration:none;font-weight:600;font-size:15px;padding:12px 22px;border-radius:8px;">Approve</a></td>`, esc(links.Approve))
		fmt.Fprintf(&sb, `<td style="padding-right:10px;"><a href="%s" style="display:inline-block;background:#b91c1c;color:#ffffff;text-decoration:none;font-weight:600;font-size:15px;padding:12px 22px;border-radius:8px;">Reject</a></td>`, esc(links.Reject))
	}
	if ev.Link != "" {
		fmt.Fprintf(&sb, `<td><a href="%s" style="display:inline-block;background:#e5e7eb;color:#111827;text-decoration:none;font-weight:600;font-size:15px;padding:12px 22px;border-radius:8px;">Open in Forge</a></td>`, esc(ev.Link))
	}
	sb.WriteString(`</tr></table>`)
	if links.Approve != "" {
		sb.WriteString(`<div style="padding-top:12px;font-size:12px;color:#6b7280;">The Approve and Reject buttons are for you only and expire in 24 hours. Each shows a confirmation page before anything is recorded.</div>`)
	}
	sb.WriteString(`</td></tr>`)
	sb.WriteString(`<tr><td style="padding:12px 24px;background:#f9fafb;font-size:12px;color:#9ca3af;">Sent by Forge CI</td></tr>`)
	sb.WriteString(`</table></td></tr></table></body></html>`)
	return sb.String()
}

// detailRows orders the details: the well-known ones first, then the rest.
func detailRows(ev Event) [][2]string {
	seen := map[string]bool{}
	var out [][2]string
	add := func(k, v string) {
		if v != "" && !seen[k] {
			seen[k] = true
			out = append(out, [2]string{k, v})
		}
	}
	if ev.Repo != "" {
		add("Repository", ev.Repo)
	}
	for _, k := range []string{"Branch", "Environment", "Started by", "Version", "URL", "Failed jobs", "Duration", "Can approve", "Needs"} {
		add(k, ev.Fields[k])
	}
	if ev.Environment != "" {
		add("Environment", ev.Environment)
	}
	for _, f := range orderedFields(ev) {
		add(f[0], f[1])
	}
	return out
}
