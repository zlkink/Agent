package host

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// pickFolder opens the system folder chooser on the machine running the host.
// It returns "" when the user cancels, and an error only when no chooser exists.
func pickFolder(start, title string) (string, error) {
	if title == "" {
		title = "Choose a folder"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	switch runtime.GOOS {
	case "windows":
		// FolderBrowserDialog must run on an STA thread.
		script := `[Console]::OutputEncoding=[Text.Encoding]::UTF8; ` +
			`Add-Type -AssemblyName System.Windows.Forms; ` +
			`$d=New-Object System.Windows.Forms.FolderBrowserDialog; ` +
			`$d.Description=$env:PROMETHEUS_PICK_TITLE; ` +
			`$d.ShowNewFolderButton=$true; ` +
			`if($env:PROMETHEUS_PICK_START -and (Test-Path -LiteralPath $env:PROMETHEUS_PICK_START)){$d.SelectedPath=$env:PROMETHEUS_PICK_START}; ` +
			`if($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK){Write-Output $d.SelectedPath}`
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-Command", script)
		cmd.Env = append(os.Environ(),
			"PROMETHEUS_PICK_START="+start,
			"PROMETHEUS_PICK_TITLE="+title,
		)
		out, err := cmd.Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("无法打开系统文件夹选择框：%v", err)
		}
		return strings.TrimSpace(string(out)), nil
	case "darwin":
		script := "POSIX path of (choose folder with prompt " + appleString(title)
		if start != "" {
			script += " default location POSIX file " + appleString(start)
		}
		script += ")"
		out, err := exec.CommandContext(ctx, "osascript", "-e", script).Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			// AppleScript exits 1 when the user presses Cancel.
			return "", nil
		}
		return strings.TrimSpace(string(out)), nil
	default:
		if bin, err := exec.LookPath("zenity"); err == nil {
			args := []string{"--file-selection", "--directory", "--title=" + title}
			if start != "" {
				args = append(args, "--filename="+strings.TrimRight(start, "/")+"/")
			}
			out, err := exec.CommandContext(ctx, bin, args...).Output()
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", nil // cancel
			}
			return strings.TrimSpace(string(out)), nil
		}
		if bin, err := exec.LookPath("kdialog"); err == nil {
			out, err := exec.CommandContext(ctx, bin, "--getexistingdirectory", start).Output()
			if err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", nil
			}
			return strings.TrimSpace(string(out)), nil
		}
		return "", fmt.Errorf("找不到 zenity 或 kdialog，无法打开系统文件夹选择框")
	}
}

func appleString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", " ")
	return `"` + s + `"`
}
