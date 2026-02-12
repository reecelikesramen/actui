# Gemini CLI - Project Knowledge & Lessons Learned

## TUI Layout & Rendering (Bubble Tea)

### 1. `bubbles/list` and Item Delegates
*   **Finding:** Trailing newlines (`
`) in `itemDelegate.Render` cause total height miscalculations.
*   **Behavior:** The list component manages spacing between items. If your delegate adds a newline, the list rendered height will double (if `Height()` is 1), causing clipping at the top of the terminal or pushing the status/help bar up into the middle of the screen.
*   **Rule:** **NEVER** include a trailing newline in `itemDelegate.Render`. Let the list component handle the line breaks.

### 2. Header Management
*   **Finding:** Built-in list titles (`SetShowTitle(true)`) can be inconsistent when switching between multiple lists or custom views.
*   **Best Practice:** Use a manual header rendered at the top level of the `View()` function and join it with the rest of the app using `lipgloss.JoinVertical(lipgloss.Left, header, content)`.
*   **Padding:** Keep `appStyle` (or your main wrapper) vertical padding at 0 if you want the header pinned to the very first line of the terminal. Use `Padding(0, 2)` for horizontal breathing room only.

### 3. Height Calculations
*   **Formula:** `listHeight = msg.Height - HeaderHeight - VerticalPadding`.
*   **Formula:** `viewport.Height = listHeight - TabHeight`. (Note: `tabStyle` with borders usually occupies 3 lines).
*   Always use `h, v := style.GetFrameSize()` to account for dynamic padding/borders in your math.

## Project Conventions
*   **Build Command:** `go build -o actui main.go`
*   **Debug Logs:** Use `tea.LogToFile("debug.log", "debug")` (already initialized in `main.go`).
