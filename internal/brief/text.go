package brief

import (
	"fmt"
	"io"
	"strings"
)

// WriteText renders the brief for a person: per flow, its findings, then its
// briefs, then what waits, with the command that decides it.
func WriteText(w io.Writer, b Brief) error {
	var sb strings.Builder
	t := b.Totals
	fmt.Fprintf(&sb, "%s · %s · %d findings · %d briefs · %d waiting\n",
		b.GeneratedAt.Format("Mon 2 Jan 15:04"), plural(t.Flows, "flow"), countItems(b), t.Briefs, t.Approvals)
	if len(b.Flows) == 0 {
		sb.WriteString("\nNothing found, nothing waiting.\n")
	}
	for _, f := range b.Flows {
		fmt.Fprintf(&sb, "\n%s\n", f.Flow)
		for _, fs := range f.Findings {
			head := fs.Title
			if head == "" {
				head = "findings"
			}
			fmt.Fprintf(&sb, "  %s (%s, %s)  → flo run show %s\n", head, plural(len(fs.Items), "item"), fs.CreatedAt.Format("2 Jan 15:04"), fs.Run)
			for _, it := range fs.Items {
				writeFinding(&sb, it)
			}
		}
		for _, br := range f.Briefs {
			title := br.Title
			if title == "" {
				title = "brief"
			}
			fmt.Fprintf(&sb, "  %s  %s  brief ready → flo run show %s\n", br.CreatedAt.Format("15:04"), title, br.Run)
		}
		if len(f.Approvals) > 0 {
			fmt.Fprintf(&sb, "  Waiting for you (%d)\n", len(f.Approvals))
			for _, a := range f.Approvals {
				title := a.Title
				if title == "" {
					title = a.Step
				}
				fmt.Fprintf(&sb, "    %s  %s  (%d of %s waiting)  → %s\n", a.ID, title, a.Waiting, plural(a.Items, "item"), a.Show)
			}
			fmt.Fprintf(&sb, "    next: %s\n", f.Approvals[0].Set)
		}
	}
	_, err := io.WriteString(w, sb.String())
	return err //nolint:wrapcheck // the writer's own error
}

func writeFinding(sb *strings.Builder, f Finding) {
	label := ""
	if f.Label != "" {
		label = f.Label + "  "
	}
	fmt.Fprintf(sb, "    %s%s", label, f.Title)
	if f.Detail != "" {
		fmt.Fprintf(sb, " — %s", f.Detail)
	}
	if f.URL != "" {
		fmt.Fprintf(sb, " <%s>", f.URL)
	}
	sb.WriteByte('\n')
}

func countItems(b Brief) int {
	n := 0
	for _, f := range b.Flows {
		for _, fs := range f.Findings {
			n += len(fs.Items)
		}
	}
	return n
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
