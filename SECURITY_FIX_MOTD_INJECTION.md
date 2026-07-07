# Security Fix: Command Injection via MOTD Shell Interpolation

## Vulnerability Summary

**CVE/Issue**: Command injection via MOTD shell interpolation  
**Severity**: High  
**Component**: `pkg/agent/runner.go`  
**Root Cause**: MOTD content was formatted as `echo %q` and sent to PTY, allowing shell command execution

## Technical Details

### Vulnerable Code (Before Fix)
```go
if cfg.MOTD != "" {
    motd := fmt.Sprintf("echo %q", cfg.MOTD)
    _ = r.Terminal.SendKeys(ctx, session, window, motd)
}
```

### Attack Vector
Go's `%q` produces double-quoted strings (e.g., `echo "text"`). When sent to a shell via PTY:
- Double quotes allow command substitution: `$(...)` 
- Double quotes allow backtick execution: `` `cmd` ``
- Double quotes allow variable expansion: `$VAR`

**Example Attack**:
```
MOTD = "$(touch /tmp/pwned)"
→ Becomes: echo "$(touch /tmp/pwned)"
→ Shell executes: touch /tmp/pwned
```

### Secure Code (After Fix)
```go
if cfg.MOTD != "" {
    escaped := strings.ReplaceAll(cfg.MOTD, "'", "'\\''")
    motd := fmt.Sprintf("printf '%%s\\n' '%s'", escaped)
    _ = r.Terminal.SendKeys(ctx, session, window, motd)
}
```

## Fix Explanation

### Why `printf '%s\n'` with Single Quotes?

1. **Single quotes prevent ALL shell expansions**:
   - `$(...)` is treated as literal text
   - `` `cmd` `` is treated as literal text
   - `$VAR` is treated as literal text
   - No globbing, no escaping needed for most characters

2. **Single quote escaping**:
   - To include a literal single quote in a single-quoted string: `'It'\''s working'`
   - This works by: ending the quote, adding escaped quote, starting new quote
   - Pattern: `'` → `'\''`

3. **printf is more reliable than echo**:
   - `echo` behavior varies across shells (especially with `-n`, `-e` flags)
   - `printf` has consistent POSIX behavior
   - `printf '%s\n'` ensures literal output with newline

### Security Properties

✅ **Prevents command substitution**: `$(whoami)` displays literally  
✅ **Prevents backtick execution**: `` `date` `` displays literally  
✅ **Prevents variable expansion**: `$HOME` displays literally  
✅ **Prevents command chaining**: `; rm -rf /` displays literally  
✅ **Prevents piping**: `| cat /etc/passwd` displays literally  
✅ **Handles single quotes**: `It's working` displays correctly  

## Testing

### Unit Tests Added

1. **TestStartWithMOTD**: Verifies MOTD is displayed using printf
2. **TestStartWithMOTDCommandInjectionPrevention**: Tests various injection attempts
3. **TestMOTDEscapingFormat**: Verifies exact command format for security

### Test Coverage

```bash
# Run tests
go test ./pkg/agent/... -v -run TestMOTD

# Expected results:
# - All MOTD tests pass
# - Command injection attempts are neutralized
# - Single quotes in messages are properly escaped
```

### Manual Verification

```bash
# Test the escaping logic directly in a shell:
printf '%s\n' '$(whoami)'          # Outputs: $(whoami)
printf '%s\n' '`date`'             # Outputs: `date`
printf '%s\n' '$HOME'              # Outputs: $HOME
printf '%s\n' 'It'\''s working'    # Outputs: It's working
```

## Impact Assessment

### Before Fix
- **Risk**: Any caller controlling `Config.MOTD` could execute arbitrary commands
- **Attack Surface**: API endpoints, configuration files, environment variables
- **Privilege**: Commands execute with agent process privileges

### After Fix
- **Risk**: Eliminated - MOTD is displayed literally
- **Functionality**: Preserved - MOTD still displays correctly
- **Compatibility**: Maintained - existing MOTD messages work unchanged

## Files Modified

1. **pkg/agent/runner.go**
   - Line 262-269: Changed MOTD formatting from `echo %q` to `printf '%s\n'`
   - Line 205-208: Updated MOTD documentation to clarify literal display

2. **pkg/agent/runner_test.go**
   - Added TestStartWithMOTDCommandInjectionPrevention
   - Added TestMOTDEscapingFormat
   - Updated TestStartWithMOTD to verify printf usage

## Recommendations

1. **Code Review**: Review other uses of `fmt.Sprintf` with shell commands
2. **Input Validation**: Consider additional validation for MOTD content
3. **Security Audit**: Audit similar patterns in `EnvPrefix` and other shell-executed fields
4. **Documentation**: Update security guidelines for shell command construction

## References

- OWASP Command Injection: https://owasp.org/www-community/attacks/Command_Injection
- CWE-78: Improper Neutralization of Special Elements used in an OS Command
- Bash Manual - Quoting: https://www.gnu.org/software/bash/manual/html_node/Quoting.html
