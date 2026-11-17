package host

import (
	"context"
	"encoding/base64"
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
		// The old FolderBrowserDialog uses SHBrowseForFolder (the tiny tree dialog).
		// Windows' current folder picker is IFileOpenDialog with FOS_PICKFOLDERS.
		script := `[Console]::OutputEncoding=[Text.Encoding]::UTF8; ` +
			`Add-Type -TypeDefinition @"
using System;
using System.IO;
using System.Runtime.InteropServices;

public static class RamiCoreFolderPicker {
  [ComImport, Guid("DC1C5A9C-E88A-4dde-A5A1-60F82A20AEF7")]
  private class FileOpenDialogRCW { }

  [ComImport, Guid("42f85136-db7e-439c-85f1-e4075d135fc8"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
  private interface IFileOpenDialog {
    [PreserveSig] int Show(IntPtr parent);
    void SetFileTypes(uint cFileTypes, IntPtr rgFilterSpec);
    void SetFileTypeIndex(uint iFileType);
    void GetFileTypeIndex(out uint piFileType);
    void Advise(IntPtr pfde, out uint pdwCookie);
    void Unadvise(uint dwCookie);
    void SetOptions(uint fos);
    void GetOptions(out uint pfos);
    void SetDefaultFolder(IShellItem psi);
    void SetFolder(IShellItem psi);
    void GetFolder(out IShellItem ppsi);
    void GetCurrentSelection(out IShellItem ppsi);
    void SetFileName([MarshalAs(UnmanagedType.LPWStr)] string pszName);
    void GetFileName([MarshalAs(UnmanagedType.LPWStr)] out string pszName);
    void SetTitle([MarshalAs(UnmanagedType.LPWStr)] string pszTitle);
    void SetOkButtonLabel([MarshalAs(UnmanagedType.LPWStr)] string pszText);
    void SetFileNameLabel([MarshalAs(UnmanagedType.LPWStr)] string pszLabel);
    void GetResult(out IShellItem ppsi);
    void AddPlace(IShellItem psi, int fdap);
    void SetDefaultExtension([MarshalAs(UnmanagedType.LPWStr)] string pszDefaultExtension);
    void Close(int hr);
    void SetClientGuid(ref Guid guid);
    void ClearClientData();
    void SetFilter(IntPtr pFilter);
    void GetResults(out IntPtr ppenum);
    void GetSelectedItems(out IntPtr ppsai);
  }

  [ComImport, Guid("43826d1e-e718-42ee-bc55-a1e261c37bfe"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
  private interface IShellItem {
    [PreserveSig] int BindToHandler(IntPtr pbc, [MarshalAs(UnmanagedType.LPStruct)] Guid bhid, [MarshalAs(UnmanagedType.LPStruct)] Guid riid, out IntPtr ppv);
    [PreserveSig] int GetParent(out IShellItem ppsi);
    [PreserveSig] int GetDisplayName(uint sigdnName, out IntPtr ppszName);
    [PreserveSig] int GetAttributes(uint sfgaoMask, out uint psfgaoAttribs);
    [PreserveSig] int Compare(IShellItem psi, uint hint, out int piOrder);
  }

  [DllImport("shell32.dll", CharSet=CharSet.Unicode, PreserveSig=true)]
  private static extern int SHCreateItemFromParsingName(
    [MarshalAs(UnmanagedType.LPWStr)] string pszPath, IntPtr pbc,
    [MarshalAs(UnmanagedType.LPStruct)] Guid riid,
    [MarshalAs(UnmanagedType.Interface)] out IShellItem ppv);

  public static string Pick(string title, string start) {
    var dlg = (IFileOpenDialog)new FileOpenDialogRCW();
    uint options;
    dlg.GetOptions(out options);
    dlg.SetOptions(options | 0x8 | 0x20 | 0x40 | 0x800); // NOCHANGEDIR | PICKFOLDERS | FORCEFILESYSTEM | PATHMUSTEXIST
    dlg.SetTitle(title);
    if (!string.IsNullOrWhiteSpace(start) && Directory.Exists(start)) {
      IShellItem folder;
      if (SHCreateItemFromParsingName(start, IntPtr.Zero, typeof(IShellItem).GUID, out folder) == 0) {
        dlg.SetFolder(folder);
        Marshal.ReleaseComObject(folder);
      }
    }
    if (dlg.Show(IntPtr.Zero) != 0) return "";
    IShellItem result;
    if (dlg.GetResult(out result) != 0 || result == null) return "";
    IntPtr name;
    if (result.GetDisplayName(0x80058000, out name) != 0 || name == IntPtr.Zero) return "";
    string path = name == IntPtr.Zero ? "" : Marshal.PtrToStringUni(name);
    if (name != IntPtr.Zero) Marshal.FreeCoTaskMem(name);
    Marshal.ReleaseComObject(result);
    Marshal.ReleaseComObject(dlg);
    return path ?? "";
  }
}
"@; ` +
			`$t=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:PROMETHEUS_PICK_TITLE_B64)); $s=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($env:PROMETHEUS_PICK_START_B64)); [RamiCoreFolderPicker]::Pick($t, $s)`
		f, err := os.CreateTemp("", "ramicore-picker-*.ps1")
		if err != nil {
			return "", err
		}
		defer os.Remove(f.Name())
		_, _ = f.WriteString("[Console]::OutputEncoding=[Text.Encoding]::UTF8\n" + script + "\n")
		_ = f.Close()
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-File", f.Name())
		cmd.Env = append(os.Environ(),
			"PROMETHEUS_PICK_START_B64="+base64.StdEncoding.EncodeToString([]byte(start)),
			"PROMETHEUS_PICK_TITLE_B64="+base64.StdEncoding.EncodeToString([]byte(title)),
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
