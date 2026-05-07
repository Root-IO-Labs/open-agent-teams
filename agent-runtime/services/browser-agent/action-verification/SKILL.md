# Action Verification Skill

After performing a browser action, verify the result before proceeding. Adapted from Nanobrowser's Validator pattern.

## When to Use

- After every significant action (click, form submit, navigation)
- Before reporting task completion
- When an action might have failed silently

## Strategy

1. After clicking a button or submitting a form:
   - Take a `browser_snapshot` or `browser_get_text`
   - Check if the page changed (new URL, new content, success message)
   - Look for error messages, validation errors, or warning banners
2. After navigation:
   - Verify the URL matches expectations
   - Check the page title or heading
   - Confirm the expected content is present
3. If verification fails:
   - Check if an error message explains the failure
   - Try the action again (up to 2 retries)
   - If still failing, report the issue with the error details

## Patterns to Check

- Success indicators: "Success", "Submitted", "Thank you", URL change, redirect
- Failure indicators: "Error", "Invalid", "Failed", form still visible, same URL
- Loading states: spinners, "Loading...", skeleton screens — wait with `browser_wait_for`
