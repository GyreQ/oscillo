// Package sysproxy 系统代理开关：capture on/off 的平台实现。
// macOS 用 networksetup，Linux 用 gsettings（GNOME 系），Windows 用注册表。
// 失败不 panic，返回错误由 CLI 提示手动操作。
package sysproxy

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// On 开启系统代理指向 addr（127.0.0.1:8788）。
func On(addr string) error {
	host, port := splitAddr(addr)
	switch runtime.GOOS {
	case "darwin":
		return darwinSet(host, port, true)
	case "windows":
		return windowsSet(host, port, true)
	default:
		return linuxSet(host, port, true)
	}
}

// Off 关闭系统代理。
func Off() error {
	switch runtime.GOOS {
	case "darwin":
		return darwinSet("", "", false)
	case "windows":
		return windowsSet("", "", false)
	default:
		return linuxSet("", "", false)
	}
}

// Restore 恢复系统代理为之前保存的值；prev 为空等价于 Off。
func Restore(prev string) error {
	if prev == "" {
		return Off()
	}
	host, port := splitAddr(prev)
	switch runtime.GOOS {
	case "darwin":
		return darwinSet(host, port, true)
	case "windows":
		return windowsSet(host, port, true)
	default:
		return linuxSet(host, port, true)
	}
}

// Read 读当前系统代理（host:port），未启用返回空串。
// 取第一个启用了 https 代理的网络服务（用户一般所有服务同值）。
func Read() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return darwinRead()
	case "windows":
		return windowsRead()
	default:
		return linuxRead()
	}
}

func splitAddr(addr string) (host, port string) {
	parts := strings.Split(addr, ":")
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return "127.0.0.1", addr
}

// darwinSet networksetup 对每个启用的网络服务设置/关闭代理。
func darwinSet(host, port string, on bool) error {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return fmt.Errorf("列出网络服务失败: %w", err)
	}
	var errs []string
	for _, line := range strings.Split(string(out), "\n") {
		svc := strings.TrimSpace(line)
		// 首行是标题；带 * 前缀的是禁用服务
		if svc == "" || strings.HasPrefix(svc, "An asterisk") || strings.HasPrefix(svc, "*") {
			continue
		}
		if on {
			// 逐服务设置 http 与 https 代理（失败收集，不中断）
			e1 := exec.Command("networksetup", "-setwebproxy", svc, host, port).Run()
			e2 := exec.Command("networksetup", "-setsecurewebproxy", svc, host, port).Run()
			if e1 != nil && e2 != nil {
				errs = append(errs, svc+": "+e1.Error())
			}
			continue
		}
		e1 := exec.Command("networksetup", "-setwebproxystate", svc, "off").Run()
		e2 := exec.Command("networksetup", "-setsecurewebproxystate", svc, "off").Run()
		if e1 != nil && e2 != nil {
			errs = append(errs, svc+": "+e1.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("部分网络服务设置失败: %s", strings.Join(errs, "; "))
	}
	return nil
}

// windowsSet 注册表设置用户级代理。
func windowsSet(host, port string, on bool) error {
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	if on {
		if err := exec.Command("reg", "add", key, "/v", "ProxyServer", "/t", "REG_SZ",
			"/d", host+":"+port, "/f").Run(); err != nil {
			return err
		}
		return exec.Command("reg", "add", key, "/v", "ProxyEnable", "/t", "REG_DWORD",
			"/d", "1", "/f").Run()
	}
	return exec.Command("reg", "add", key, "/v", "ProxyEnable", "/t", "REG_DWORD",
		"/d", "0", "/f").Run()
}

// linuxSet gsettings（GNOME 系桌面）。
func linuxSet(host, port string, on bool) error {
	mode := "none"
	if on {
		mode = "manual"
		if err := exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "host", host).Run(); err != nil {
			return err
		}
		if err := exec.Command("gsettings", "set", "org.gnome.system.proxy.https", "port", port).Run(); err != nil {
			return err
		}
		if err := exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "host", host).Run(); err != nil {
			return err
		}
		if err := exec.Command("gsettings", "set", "org.gnome.system.proxy.http", "port", port).Run(); err != nil {
			return err
		}
	}
	return exec.Command("gsettings", "set", "org.gnome.system.proxy", "mode", mode).Run()
}

// darwinServices 列出启用的网络服务名。
func darwinServices() ([]string, error) {
	out, err := exec.Command("networksetup", "-listallnetworkservices").Output()
	if err != nil {
		return nil, fmt.Errorf("列出网络服务失败: %w", err)
	}
	var svcs []string
	for _, line := range strings.Split(string(out), "\n") {
		svc := strings.TrimSpace(line)
		if svc == "" || strings.HasPrefix(svc, "An asterisk") || strings.HasPrefix(svc, "*") {
			continue
		}
		svcs = append(svcs, svc)
	}
	return svcs, nil
}

// darwinRead networksetup -getsecurewebproxy 逐服务探测，取第一个启用的。
func darwinRead() (string, error) {
	svcs, err := darwinServices()
	if err != nil {
		return "", err
	}
	for _, svc := range svcs {
		out, err := exec.Command("networksetup", "-getsecurewebproxy", svc).Output()
		if err != nil {
			continue
		}
		var host, port string
		enabled := false
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "Enabled:"):
				enabled = strings.Contains(line, "Yes")
			case strings.HasPrefix(line, "Server:"):
				host = strings.TrimSpace(strings.TrimPrefix(line, "Server:"))
			case strings.HasPrefix(line, "Port:"):
				port = strings.TrimSpace(strings.TrimPrefix(line, "Port:"))
			}
		}
		if enabled && host != "" {
			return host + ":" + port, nil
		}
	}
	return "", nil
}

// windowsRead reg query 读用户级代理。
func windowsRead() (string, error) {
	key := `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	out, err := exec.Command("reg", "query", key, "/v", "ProxyServer").Output()
	if err != nil {
		return "", nil // 未设置
	}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "ProxyServer"); i >= 0 {
			v := strings.TrimSpace(strings.SplitN(line, "REG_SZ", 2)[1])
			if v != "" && v != "127.0.0.1:8788" {
				return v, nil
			}
		}
	}
	return "", nil
}

// linuxRead gsettings 读 GNOME 代理。
func linuxRead() (string, error) {
	mode, err := exec.Command("gsettings", "get", "org.gnome.system.proxy", "mode").Output()
	if err != nil || !strings.Contains(string(mode), "manual") {
		return "", nil
	}
	host, _ := exec.Command("gsettings", "get", "org.gnome.system.proxy.https", "host").Output()
	port, _ := exec.Command("gsettings", "get", "org.gnome.system.proxy.https", "port").Output()
	h := strings.Trim(strings.TrimSpace(string(host)), "'")
	p := strings.TrimSpace(string(port))
	if h == "" || h == "''" || p == "" || p == "0" {
		return "", nil
	}
	return h + ":" + p, nil
}
