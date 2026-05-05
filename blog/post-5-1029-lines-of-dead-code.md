# 1,029 Lines of Dead Code: What We Learned Building an AI Verification System

## We built a system to catch AI coding errors. It had more bugs per line than the code it was supposed to verify.

---

Here's a sentence that should make you uncomfortable: your AI verification system probably isn't running.

I know because ours wasn't. For weeks. We shipped 1,029 lines of Go implementing five verification checks -- file integrity, test execution, syntax validation, task alignment, and input validation. We wrote tests. We documented it. Workers ran `oat worker verify` before every PR and reported passing scores.

The scores came from the wrong function.

## The wiring bug

OAT has two verify functions. `verifyWorker` is the comprehensive five-check system -- 1,029 lines of Go that detect truncated files, run tests, validate syntax across four languages, and check task alignment via LLM review. `simpleVerify` is a 150-line fallback that checks for duplicate lines and not much else.

The CLI command `oat worker verify` was wired to `simpleVerify`.

One thousand twenty-nine lines of verification logic, never executed in production. Every agent that "passed verification" had been evaluated by the lightweight version. The comprehensive system was dead code from the day it shipped.

We found this when we traced why a worker's obviously broken output passed verification. The answer: it hadn't been verified. Not really.

## Six more bugs

Fixing the wiring exposed six more problems. Each one is a pattern that will show up in any AI verification system, and probably already has.

### Bug 2: The placeholder task

Task alignment checks whether the implementation matches the original task description. It's weighted at 15% of the total score. Our `getOriginalTask` function was supposed to retrieve the worker's assigned task from the daemon state.

It returned the string `"VERIFIED_TASK_FOR_agent_IN_repo"`.

That's a format-string placeholder that was never replaced with actual values. Task alignment always scored 70/100 because the LLM judge compared the implementation against a meaningless string and gave it a middling score. Every worker passed this check by default.

### Bug 3: The empty diff

File integrity checks look at modified files for truncation, corruption, and duplicate blocks. The function `getModifiedFiles` runs `git diff HEAD` to find what changed.

Workers are instructed to commit before verifying. After the commit, `git diff HEAD` returns nothing. There are no modified files. Every file integrity check passed vacuously because there were no files to check.

The fix was to diff against the merge base instead of HEAD. But the deeper lesson is that **verification systems need to account for the workflow they're embedded in.** The verification step ran after the commit step. The commit step eliminated the evidence the verification step needed. Nobody noticed because the output ("0 issues found") looked correct.

### Bug 4: The Ellipsis problem

Our truncation detector flagged files ending with `...` as potentially truncated. This is a real LLM failure mode -- models sometimes generate partial output with trailing ellipsis.

Python's `Ellipsis` literal is `...`. JavaScript's spread operator uses `...`. Both triggered false positives. In a Python codebase, almost every `__init__.py` file would fail verification because `pass` is often written as `...` in stub files.

This one's straightforward, but it illustrates the tension in verification: **the more aggressively you flag potential issues, the more false positives you generate, and the faster developers learn to ignore verification results.** A verification system that cries wolf is worse than no verification at all because it creates a false sense of security.

### Bug 5: The node -c misunderstanding

Syntax validation runs language-specific checks: `python -m py_compile` for Python, `go build -o /dev/null` for Go, `bash -n` for shell, and `node -c` for JavaScript.

`node -c` does not validate files. It evaluates its argument as JavaScript source code. When we ran `node -c test.js`, Node parsed the *string* `"test.js"` -- which is a valid JavaScript expression (an identifier followed by a member access). Every JavaScript file passed syntax checking because we were checking the filename, not the file contents.

The correct invocation is `node --check test.js` (long flag) or `node -c "$(cat test.js)"` (pipe the content). We went with `node --check`.

### Bug 6: The backwards dedup

When detecting duplicate code blocks, the dedup logic removed the first occurrence and preserved the second. This means it kept the copy and deleted the original.

In most codebases, the first occurrence of a code block is the real one and the second is the accidental duplicate (from an LLM regenerating a section it already produced). Removing the original could delete import statements, function definitions, or class declarations that other code depends on.

The fix was a one-line change (remove from the second occurrence instead of the first). But the bug had been there since the system was written, invisible because the system was never executing.

### Bug 7: The auto-fix that destroyed code

`oat worker verify --fix` was designed to automatically repair common issues. The global line deduplicator removed all repeated lines in a file -- not just consecutive duplicates, but any line that appeared more than once anywhere in the file.

This killed:
- Repeated import statements (`from typing import Optional` appearing in multiple blocks)
- Test assertions with identical structure (`assert result == expected`)
- Matching HTML/XML tags (`<div>` ... `</div>` ... `<div>` ... `</div>`)
- Blank lines (every blank line after the first was removed)

The "fix" was more destructive than the bugs it was supposed to repair. We scoped the deduplicator to only flag consecutive duplicate blocks of 3+ lines, which catches real LLM duplication without touching intentional repetition.

## What seven bugs in a safety net teach you

The irony is obvious: the system designed to catch AI errors had more bugs per line than the code it was supposed to verify. But the irony isn't the lesson. The lesson is structural.

**Verification systems have the same failure modes as the code they verify.** They're written by the same developers, with the same assumptions, under the same time pressure. If your codebase has a 2% bug rate, your verification system probably does too. And verification systems get tested less rigorously because everyone assumes the safety net works.

**Plausible output is the enemy of correctness.** Every one of these bugs produced output that looked right. "0 issues found" is correct when there are no issues. It's also correct when the issues aren't being checked. "Task alignment: 70/100" is a plausible score. It's also the score you get when comparing against a placeholder string. The verification system never crashed, never threw errors, never produced obviously wrong output. It just quietly verified nothing.

**Verify the verifier.** We should have had a test case with a known-bad file that must fail verification. If we had, we'd have caught the wiring bug on day one. The file would have passed when it should have failed, and we'd have traced the problem to `simpleVerify` immediately.

We've since added exactly this: a test suite with intentionally broken inputs that must produce specific verification failures. If the verification system ever stops catching known bugs, the tests fail. The gate has a gate.

## The performance data

After fixing all seven bugs, we measured the verification system's impact on benchmark scores:

The system catches real issues now. File integrity alone (35% of the weighted score) prevents approximately 40% of common LLM coding errors: truncated files, duplicate code blocks, and files with missing content. Test execution (25% weight) catches breaking changes before they reach a PR. Syntax validation (20% weight) catches parse errors across Python, JavaScript, Go, and shell scripts.

Execution time: ~3ms for the fast checks (file integrity, syntax). Variable for test execution depending on the project's test suite.

The verification gate in the worker template requires a score of 70/100 to proceed with PR creation. Workers that fail have three options: fix the issues and re-verify, request an independent verification agent, or force-skip (which is logged and visible to the supervisor).

## The uncomfortable question

How many AI verification systems in production right now have a version of our wiring bug?

You've got a code review bot. It "runs on every PR." The dashboard says 100% coverage. But have you checked that it's actually executing the checks it claims to? That it's reading the right diff? That its judgments are based on the actual code and not a placeholder or a cached result?

Plausible output is the enemy of correctness. The only way to know your safety net works is to throw something at it that must be caught, and verify that it is.

---

*This is part of a series on building multi-agent systems. OAT is open source at [github.com/Root-IO-Labs/open-agent-teams](https://github.com/Root-IO-Labs/open-agent-teams).*
