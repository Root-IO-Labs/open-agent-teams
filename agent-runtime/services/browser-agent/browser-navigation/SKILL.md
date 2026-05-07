# Browser Navigation Skill

Navigate multi-step flows in the browser: login sequences, multi-page traversal, form wizards, and authenticated session management.

## When to Use

- Login sequences requiring username → password → submit → verify
- Multi-page navigation flows (e.g., paginated results, wizard forms)
- Following redirect chains
- Handling OAuth popup flows across tabs

## Strategy

1. Start with `browser_navigate` to the target URL
2. Use `browser_dismiss_overlay` to clear cookie banners
3. Take a `browser_snapshot` to understand the page
4. Identify form fields and buttons via element refs
5. Fill fields with `browser_type` or `browser_fill`, then click submit
6. After navigation, verify success with `browser_get_text` or `browser_snapshot`
7. If a new tab opens (OAuth), use `browser_tabs` to track and switch

## Error Recovery

- If login fails, check for error messages in the snapshot before retrying
- If a CAPTCHA appears, stop and report via `browser_detect_captcha`
- If the page doesn't change after clicking, verify the button ref is correct
- Use `browser_wait_for` to wait for page transitions
