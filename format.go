// format.go

package logger

import (
	"fmt"
	"html"
	"strings"
)

// formatTelegram renders one log entry for its channel.
//
// Output is Telegram HTML (see TelegramClient.SendLog, which sets the parse
// mode). HTML rather than Markdown because only < > & need escaping, whereas
// Markdown breaks on any stray _ or * in a venue name or error string.
//
// The shape is deliberately one fact per line, most specific first:
//
//	🟢 BET · pegasus
//
//	Successfully sent bet notification
//
//	🏇 ROCKHAMPTON R7 · Fixed Win
//	🐎 Runner #1 Brank Daddy
//	💰 Target $240.00
//	⚙️ pegasus_a1b2c3
func formatTelegram(e Entry) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "%s <b>%s</b>", levelIcon(e.LogType), escapeText(e.LogType))
	if e.Application != "" {
		fmt.Fprintf(&sb, " · %s", escapeText(e.Application))
	}

	if msg := strings.TrimSpace(e.Message); msg != "" {
		fmt.Fprintf(&sb, "\n\n%s", escapeText(msg))
	}

	details := detailLines(e)
	if len(details) > 0 {
		sb.WriteString("\n\n")
		sb.WriteString(strings.Join(details, "\n"))
	}

	if err := strings.TrimSpace(e.Error); err != "" {
		fmt.Fprintf(&sb, "\n\n❌ <b>Error</b>\n<pre>%s</pre>", escapeText(err))
	}

	return sb.String()
}

// detailLines builds the per-fact block under the message. Fields come
// straight off the original payloads rather than out of the JSONB — they're
// right there, so there's no reason to marshal and re-parse them.
func detailLines(e Entry) []string {
	var details []string

	r := e.RaceDetails
	b := e.bet

	if r != nil && r.Venue != "" {
		line := "🏇 <b>" + escapeText(r.Venue)
		if r.RaceNumber != 0 {
			line += fmt.Sprintf(" R%d", r.RaceNumber)
		}
		line += "</b>"
		if b != nil && b.Market != "" {
			line += " · " + escapeText(b.Market)
		}
		details = append(details, line)
	}

	if r != nil && (r.RunnerNumber != 0 || r.RunnerName != "") {
		line := "🐎 Runner"
		if r.RunnerNumber != 0 {
			line += fmt.Sprintf(" <b>#%d</b>", r.RunnerNumber)
		}
		if r.RunnerName != "" {
			line += " " + escapeText(r.RunnerName)
		}
		details = append(details, line)
	}

	if b != nil {
		if b.Stake != 0 {
			line := "💵 Stake " + formatMoney(b.Stake)
			if b.Odds != 0 {
				line += fmt.Sprintf(" @ %.2f", b.Odds)
			}
			details = append(details, line)
		}
		if b.TargetLia != 0 {
			details = append(details, "💰 Target "+formatMoney(b.TargetLia))
		}
		if b.RaceState != "" {
			details = append(details, "📍 "+escapeText(b.RaceState))
		}
		if b.Result != "" {
			details = append(details, "🏁 "+escapeText(b.Result))
		}
		if b.Endpoint != "" {
			details = append(details, "🔌 "+escapeText(b.Endpoint))
		}
	}

	if e.Username != "" {
		details = append(details, "👤 "+escapeText(e.Username))
	}
	if e.ProcessID != "" {
		details = append(details, "⚙️ "+escapeText(e.ProcessID))
	}

	return details
}

// formatSuppressed summarises repeats that the dedupe window swallowed.
func formatSuppressed(sample Entry, count int) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "%s <b>%s</b>", levelIcon(sample.LogType), escapeText(sample.LogType))
	if sample.Application != "" {
		fmt.Fprintf(&sb, " · %s", escapeText(sample.Application))
	}
	fmt.Fprintf(&sb, "\n\n🔁 <b>%d more</b> identical to:\n%s", count, escapeText(strings.TrimSpace(sample.Message)))

	if err := strings.TrimSpace(sample.Error); err != "" {
		fmt.Fprintf(&sb, "\n<pre>%s</pre>", escapeText(err))
	}
	return sb.String()
}

func levelIcon(logType string) string {
	switch logType {
	case "ERROR":
		return "🔴"
	case "WARN":
		return "🟠"
	case "BET":
		return "🟢"
	case "INFO":
		return "🔵"
	default:
		return "⚪"
	}
}

func escapeText(s string) string { return html.EscapeString(s) }

func formatMoney(v float64) string {
	if v < 0 {
		return fmt.Sprintf("-$%.2f", -v)
	}
	return fmt.Sprintf("$%.2f", v)
}
