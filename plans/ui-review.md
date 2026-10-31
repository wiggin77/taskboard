# UI review

Reviewed 2026-10-31 in Chrome against real data (Reviews 20, Other repos 29, In progress 1, Not started 100, Done 25), in dark mode, light mode, at phone width and on the settings page.

The board looks tidy, but it doesn't answer its main question quickly: "what do I need to act on now?" With this data, the one in-progress task sits about 50 rows down, below 49 PRs that belong to other people. Most of the gains come from changing what gets emphasis, not from restyling.

## Suggestions, in priority order

### 1. Put your own work first and fold away the long lists

- **Reviews** are requests for you, so they belong near the top.
- **Other repos** is mostly "Reviewed, waiting on author", some from 2024. That's reference, not work. Fold it away like Done.
- **Not started** should also be folded, or show the 10 most recently updated with a "Show all" link.
- With those changes, the visible page drops from about 175 rows to about 30.

### 2. Use color only where you need to act

- Amber marks every "Review requested" label, every 2–7-day-old date, and the "Needs you" link. Red marks dates on other people's PRs from 2024. With that much color, none of it stands out.
- Rule: color the dates on your own active tasks only, keep them grey everywhere else (Done already works this way), and keep tinted labels for statuses that need you.

### 3. Stop showing each task's stage three times

- Each row shows its stage with a colored bar on the left, a colored label, and the dot on the progress track.
- Keep the label and the track. Show the left bar only on "Needs you" rows, so scanning down the left edge shows only what needs you.

### 4. Show the progress track only once work has started

- On Not started rows the track is an empty line, repeated for every row.
- Not started rows could be a single line: key, title, Jira status, date.
- The track is the board's one distinctive element. It means more if it only appears on tasks that are under way.

### 5. Make PR rows one line

- Reviews and Other repos rows are two lines (about 70px each). One line would do: title, repo and number, author, then status and date on the right. That halves their height.

### 6. Treat ticket keys the same everywhere

- Task rows show the key in a small grey box (e.g. `MM-71094`), but PR titles keep the raw prefix: `[MM-71400] Soft-delete…` or `MM-71261: Update…`.
- Pull the key out with the existing `findKey` into the same box and strip it from the title.

### 7. Fix confusing wording

- "Jira Open" reads like a button that opens Jira, but "Open" is the ticket's status. "Jira: Open", or showing the status next to the stage, fixes it.
- With 100 tasks in Not started, its rows should drop the "Not started" status label as well.

### 8. Make fewer things round

- The section links, counts, status labels, icon buttons and settings buttons are all fully rounded, so nothing looks more important than anything else.
- Keep the rounded shape for status labels only. Make counts plain numbers and the section links quiet text links.

### 9. Pick a font on purpose

- The CSS asks for "Inter" first but never loads it. The board renders in Inter only on machines where it happens to be installed, and in the system font everywhere else.
- For a local tool, choose `system-ui` deliberately: no font download, and it matches the OS. To look the same everywhere instead, include a font in the binary.

### 10. Settings page

- Each repo row repeats the "Name / Local path / Plans dir" labels. Show them once, as a header row.
- The local path column cuts off long paths after about 30 characters. Give paths more width, or put them on their own line.

### Smaller items

- The stage labels above the track (Ticket, Plan, Code, …) are 11px grey text, which is hard to read in dark mode.
- At phone width the section links scroll sideways with no hint that more are hidden.

## Proposed direction

Keep the current colors and fonts, which are calm and work in both light and dark mode. Make the progress track the one distinctive element and keep everything around it quiet. Then add a one-line summary under the header that answers the page's question, e.g. "2 need you, 20 reviews waiting", or "Nothing needs you" when that's true.

Items 1–4 are the biggest win for the effort: mostly template and CSS changes, plus a little grouping logic in `server.go`.
