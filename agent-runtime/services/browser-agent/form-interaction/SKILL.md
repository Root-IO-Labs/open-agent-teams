# Form Interaction Skill

Fill and submit web forms: text inputs, dropdowns, checkboxes, radio buttons, file uploads, and multi-step form wizards.

## When to Use

- Filling out contact forms, registration forms, search forms
- Selecting options from dropdowns
- Checking/unchecking checkboxes and radio buttons
- Uploading files via file inputs
- Submitting forms and verifying success

## Strategy

1. Take a `browser_snapshot` to identify all form elements
2. For text fields: use `browser_fill` (faster) or `browser_type` (character-by-character, for fields with input validation)
3. For dropdowns: use `browser_select_option` with the option value
4. For checkboxes: use `browser_check` / `browser_uncheck`
5. For file uploads: use `browser_file_upload`
6. Click the submit button with `browser_click`
7. Verify submission with `browser_snapshot` or `browser_get_text`

## Safety

- NEVER fill in credit card numbers, SSNs, or API keys (blocked by the safety layer)
- If a form asks for sensitive financial data, stop and report
- Verify you're on the correct page before filling forms
