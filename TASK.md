## Context

Issue #86, follow-up from #77. The "A CRBL Technologies product" footer on the
client dashboard requires scrolling to see on shorter viewports. The prior
flexbox fix (commit f71297d) improved it but the footer is still inside the
`.wrap` div, so it doesn't pin to the viewport bottom.

## Goal

Make the footer always visible at the bottom of the viewport without scrolling,
using the classic CSS sticky-footer pattern that is already partially in place.

## Constraints / guardrails

- DO NOT delete or modify code not mentioned in this task.
- Only touch `cmd/client/ui.go`, only the HTML template section.
- Do not change any Go logic, JavaScript, or server-side code.
- Keep the footer's visual appearance (text, link, colors, font size) identical.

## Tasks

- [ ] In `cmd/client/ui.go` around line 439-442, move the footer `<div>` so it
      is **outside** the `.wrap` div (i.e., a direct child of `<body>`).
      Currently the closing `</div>` of `.wrap` is at line 442 (after the footer).
      Move it to **before** the footer div so the structure becomes:

      ```
      <body>          ← flex column, min-h-100vh (already set)
        <div class="wrap">  ← flex:1 (already set)
          ...panels...
        </div>               ← close .wrap BEFORE footer
        <footer>...</footer> ← now a direct child of body
      </body>
      ```

- [ ] Add `margin-top: auto;` to the footer div's inline style so it is pushed
      to the bottom of the viewport when content is short.

## Tests

No new Go tests needed — this is a CSS-only change in an HTML template literal.
Verify visually or by inspecting the rendered HTML.

## Acceptance criteria

1. The footer div is a direct child of `<body>`, not nested inside `.wrap`.
2. The footer has `margin-top: auto` in its style.
3. On a tall viewport with little content, the footer sits at the bottom of the
   viewport (not below the fold).
4. On a short viewport with lots of content, the footer appears after the content
   (natural document flow, no overlap).
5. No other code is changed.

## Verification

```bash
go build ./...
go vet ./...
```
