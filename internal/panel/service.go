// Panel service management: Windows scheduled-task wiring for boot autostart
// and the periodic watchdog. The scheduled task itself is a system-level
// change, so it is only created/deleted when the operator flips the switch in
// the panel — never silently at startup. Defaults mirror the operator's
// established preference: autostart ON, watchdog OFF.
package panel

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

const (
	// KeyAutostart persists the boot-autostart preference ("true"/"false").
	KeyAutostart = "service_autostart_enabled"
	// KeyWatchdog persists the every-5-minutes watchdog preference.
	KeyWatchdog = "service_watchdog_enabled"

	bootTaskName     = "BargainProxy_BootStart"
	watchdogTaskName = "BargainProxy_Watchdog"
)

// vbsScript is the absolute path to scripts\autostart.vbs, captured at boot
// by the server binary (which knows its own layout); empty when unknown, in
// which case task registration reports a clear error instead of guessing.
var vbsScript string

// SetAutostartScript records the absolute path of scripts/autostart.vbs.
func SetAutostartScript(path string) { vbsScript = path }

// AutostartEnabled reports the persisted autostart preference (default true).
func (p *Panel) AutostartEnabled() bool {
	if v, ok, _ := p.store.GetSetting(KeyAutostart); ok {
		return v != "false"
	}
	return true
}

// WatchdogEnabled reports the persisted watchdog preference (default false).
func (p *Panel) WatchdogEnabled() bool {
	if v, ok, _ := p.store.GetSetting(KeyWatchdog); ok {
		return v == "true"
	}
	return false
}

// ServiceView is the panel-facing snapshot: preferences plus the actual
// scheduled-task state as Windows reports it.
func (p *Panel) ServiceView() map[string]any {
	return map[string]any{
		"autostart_enabled":   p.AutostartEnabled(),
		"watchdog_enabled":    p.WatchdogEnabled(),
		"boot_task_state":     scheduledTaskState(bootTaskName),
		"watchdog_task_state": scheduledTaskState(watchdogTaskName),
		"supported":           runtime.GOOS == "windows",
	}
}

// SetAutostart persists the preference and creates/deletes the boot task.
func (p *Panel) SetAutostart(enabled bool) error {
	if err := p.store.SetSetting(KeyAutostart, boolStr(enabled)); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("scheduled tasks are only supported on Windows")
	}
	if enabled {
		return upsertScheduledTask(bootTaskName, "ONLOGON", "")
	}
	return deleteScheduledTask(bootTaskName)
}

// SetWatchdog persists the preference and creates/deletes the 5-minute task.
func (p *Panel) SetWatchdog(enabled bool) error {
	if err := p.store.SetSetting(KeyWatchdog, boolStr(enabled)); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return fmt.Errorf("scheduled tasks are only supported on Windows")
	}
	if enabled {
		return upsertScheduledTask(watchdogTaskName, "MINUTE", "/MO 5")
	}
	return deleteScheduledTask(watchdogTaskName)
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// EnsureBootTask reconciles the boot task with the persisted preference at
// gateway startup: the default is autostart ON, so a fresh install gets the
// task registered without the operator having to find the switch. Errors are
// logged by the caller and never fatal (a missed task registration must not
// stop the gateway).
func (p *Panel) EnsureBootTask() error {
	if runtime.GOOS != "windows" || vbsScript == "" {
		return nil
	}
	// A healthy existing task needs no work.
	if st := scheduledTaskState(bootTaskName); st == "就绪" || st == "运行中" {
		return nil
	}
	if !p.AutostartEnabled() {
		// Operator turned it off; respect that even though the task is absent.
		return nil
	}
	return upsertScheduledTask(bootTaskName, "ONLOGON", "")
}
func vbsPath() (string, error) {
	return exec.LookPath("wscript.exe")
}

func upsertScheduledTask(name, schedule, extra string) error {
	if vbsScript == "" {
		return fmt.Errorf("autostart script path unknown (server started from an unusual layout); run scripts\\start-all.cmd from the project root")
	}
	wscript, err := vbsPath()
	if err != nil {
		return fmt.Errorf("wscript.exe not found: %w", err)
	}
	task := fmt.Sprintf(`"%s" //nologo "%s"`, wscript, vbsScript)
	cmd := exec.Command("schtasks", "/Create", "/F", "/SC", schedule)
	if extra != "" {
		cmd.Args = append(cmd.Args, strings.Fields(extra)...)
	}
	cmd.Args = append(cmd.Args, "/TN", name, "/TR", task)
	if out, err := cmd.CombinedOutput(); err != nil {
		// Already exists: update instead of failing.
		if strings.Contains(string(out), "already exists") || strings.Contains(string(out), "已存在") {
			upd := exec.Command("schtasks", "/Change", "/TN", name, "/TR", task)
			if out2, err2 := upd.CombinedOutput(); err2 != nil {
				return fmt.Errorf("schtasks change: %v: %s", err2, out2)
			}
			return nil
		}
		return fmt.Errorf("schtasks create: %v: %s", err, out)
	}
	return nil
}

func deleteScheduledTask(name string) error {
	cmd := exec.Command("schtasks", "/Delete", "/F", "/TN", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "does not exist") || strings.Contains(string(out), "不存在") {
			return nil // already gone: idempotent delete
		}
		return fmt.Errorf("schtasks delete: %v: %s", err, out)
	}
	return nil
}

// scheduledTaskState reports the task state localized to Chinese for the
// panel: 就绪 / 运行中 / 已禁用 / ""（未注册）. schtasks prints its console
// output in the system ANSI codepage (GBK on zh-CN), which Go would read as
// mojibake — so the raw status is never surfaced; instead presence + the
// task's enabled flag decide between the three known states.
func scheduledTaskState(name string) string {
	if runtime.GOOS != "windows" {
		return ""
	}
	if err := exec.Command("schtasks", "/Query", "/TN", name).Run(); err != nil {
		return "" // not registered (or query failed): treat as absent
	}
	// Registered. The enabled flag lives in the same CSV status column, but
	// decoding GBK just for it is fragile; query the task XML instead, which
	// schtasks emits in UTF-8-friendly XML with an <Enabled> element.
	out, err := exec.Command("schtasks", "/Query", "/TN", name, "/XML").CombinedOutput()
	if err == nil && strings.Contains(string(out), "<Enabled>false</Enabled>") {
		return "已禁用"
	}
	return "就绪"
}
