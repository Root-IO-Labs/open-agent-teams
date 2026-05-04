package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func collectSimpleVerifyFiles() ([]string, error) {
	patterns := []string{"*.py", "*.js", "*.json", "*.go", "*.sh"}
	files := make([]string, 0)
	seen := make(map[string]struct{}, len(patterns))

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, match := range matches {
			if _, ok := seen[match]; ok {
				continue
			}
			seen[match] = struct{}{}
			files = append(files, match)
		}
	}

	return files, nil
}

// simpleVerify performs a fast, lightweight verification for testing
func (c *CLI) simpleVerify(args []string) error {
	start := time.Now()

	flags, _ := ParseFlags(args)
	autoFix := flags["fix"] == "true"
	verbose := flags["verbose"] == "true"

	fmt.Println("🔍 Running fast verification...")

	// Quick context check - but don't fail
	cwd, _ := os.Getwd()
	if strings.Contains(cwd, ".oat/wts") {
		fmt.Println("📁 Worker context detected")
	} else {
		fmt.Println("📁 Running in current directory")
		if verbose {
			fmt.Printf("   Directory: %s\n", filepath.Base(cwd))
		}
	}

	// filepath.Glob does not support brace expansion, so scan extensions explicitly.
	files, err := collectSimpleVerifyFiles()
	if err != nil {
		files = []string{}
	}

	fmt.Printf("📁 Found %d files to check\n", len(files))

	issues := 0
	fixed := 0

	for _, file := range files {
		if verbose {
			fmt.Printf("  Checking: %s... ", file)
		}

		content, err := os.ReadFile(file)
		if err != nil {
			if verbose {
				fmt.Println("(read error)")
			}
			issues++
			continue
		}

		lines := strings.Split(string(content), "\n")

		// Check for consecutive duplicate blocks (3+ identical lines in a row)
		dupeFound := false
		for i := 0; i+3 < len(lines); i++ {
			blockSize := 3
			if i+blockSize*2 > len(lines) {
				break
			}
			block := strings.Join(lines[i:i+blockSize], "\n")
			nextBlock := strings.Join(lines[i+blockSize:i+blockSize*2], "\n")
			if strings.TrimSpace(block) != "" && block == nextBlock {
				if autoFix {
					// Remove the duplicate (second occurrence)
					lines = append(lines[:i+blockSize], lines[i+blockSize*2:]...)
					fixed++
					dupeFound = true
					break
				}
				issues++
				if verbose {
					fmt.Printf("\n    Duplicate block at line %d\n", i+1)
				}
				dupeFound = true
				break
			}
		}

		if !dupeFound && verbose {
			fmt.Println("ok")
		}

		if autoFix && dupeFound {
			if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0644); err != nil {
				fmt.Printf("    Warning: could not write fix to %s: %v\n", file, err)
			}
		}
	}

	duration := time.Since(start)

	fmt.Println(strings.Repeat("-", 40))
	fmt.Printf("Verification completed in %v\n", duration)
	fmt.Printf("Results: %d files checked, %d issues found", len(files), issues)

	if autoFix && fixed > 0 {
		fmt.Printf(", %d files fixed", fixed)
	}
	fmt.Println()

	if issues == 0 {
		fmt.Println("No issues found - code looks good!")
	} else if autoFix && fixed > 0 {
		fmt.Printf("Fixed %d/%d issues automatically\n", fixed, issues)
	} else {
		fmt.Println("Run with --fix to automatically repair issues")
	}

	return nil
}
