' BargainProxy silent watchdog: no console window at all.
' If the gateway port 13140 is not listening, silently start the stack
' (portable PostgreSQL + gateway). Idempotent: exits immediately otherwise.
' Registered by the panel (POST /panel/api/service/autostart) as a scheduled
' task; watchdog cadence (none / every 5 min) is also panel-controlled.
Set sh = CreateObject("Wscript.Shell")
Set fso = CreateObject("Scripting.FileSystemObject")
base = fso.GetParentFolderName(fso.GetParentFolderName(WScript.ScriptFullName))

logDir = base & "\logs"
If Not fso.FolderExists(logDir) Then
    fso.CreateFolder(logDir)
End If

Set exec = sh.Exec("cmd /c netstat -ano | findstr :13140 | findstr LISTENING")
Do While exec.Status = 0
    WScript.Sleep 100
Loop
busy = (Len(exec.StdOut.ReadAll()) > 0)

If Not busy Then
    sh.Run "cmd /c cd /d """ & base & """ && scripts\start-all.cmd >> """ & logDir & "\autostart.log"" 2>&1", 0, False
End If
