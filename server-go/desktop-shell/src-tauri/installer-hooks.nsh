; Runs before files are copied. The bundled node.exe (the agent backend) can still be running from the
; previous version and Windows will not let it be overwritten, which fails the install with
; "Error opening file for writing". Stop every process that runs from the install folder first.
!macro NSIS_HOOK_PREINSTALL
  ; Kill the desktop app and the Go host by name as well as by install path:
  ; a process left running from an older install silently keeps the old
  ; RamiCore Desktop.exe locked while NSIS still reports success.
  nsExec::Exec 'cmd /c taskkill /IM "RamiCore Desktop.exe" /T /F >nul 2>&1 & taskkill /IM "RamiCore.exe" /T /F >nul 2>&1'
  Pop $0
  Sleep 1200
  nsExec::Exec `powershell -NoProfile -NonInteractive -WindowStyle Hidden -Command "$$d='$INSTDIR'.TrimEnd('\'); Get-CimInstance Win32_Process | Where-Object { $$_.ExecutablePath -and $$_.ExecutablePath.StartsWith($$d + '\', [StringComparison]::OrdinalIgnoreCase) -and $$_.ProcessId -ne $$PID } | ForEach-Object { Stop-Process -Id $$_.ProcessId -Force -ErrorAction SilentlyContinue }; Start-Sleep -Milliseconds 800"`
  Pop $0
!macroend
