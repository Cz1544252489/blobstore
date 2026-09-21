// Package install puts the running blobstore binary in place as a systemd
// service on a Linux server, or takes it out again. It touches only the
// service's own files and never any Trilium credential.
package install

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	binPath  = "/usr/local/bin/blobstore"
	confDir  = "/etc/blobstore"
	confPath = confDir + "/config"
	unitPath = "/etc/systemd/system/blobstore.service"
	dataDir  = "/var/lib/blobstore"
)

// Install copies this binary to binPath, writes the unit, creates the config
// when there is none, and (re)starts the service. An existing config is kept
// as it is, so an upgrade keeps its tokens and addresses.
func Install(args []string, unit []byte) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:37862", "监听地址(只用于新生成的配置)")
	publicBase := fs.String("public-base", "", "写进返回地址的前缀,不带结尾的斜杠(只用于新生成的配置;默认 http://<addr>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := preflight(); err != nil {
		return err
	}

	if err := copySelf(); err != nil {
		return fmt.Errorf("安装程序失败:%w", err)
	}
	fmt.Println("程序:" + binPath)

	if err := os.MkdirAll(confDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(confPath); err == nil {
		fmt.Println("配置已存在,保留不动:" + confPath)
	} else if errors.Is(err, os.ErrNotExist) {
		base := strings.TrimSuffix(*publicBase, "/")
		if base == "" {
			base = "http://" + *addr
		}
		if err := writeConfig(*addr, base); err != nil {
			return fmt.Errorf("生成配置失败:%w", err)
		}
		fmt.Println("已生成配置:" + confPath)
	} else {
		return err
	}

	if err := os.WriteFile(unitPath, unit, 0o644); err != nil {
		return fmt.Errorf("写单元文件失败:%w", err)
	}
	fmt.Println("单元:" + unitPath)
	// restart rather than "enable --now": on an upgrade the service is already running and
	// --now would leave it on the binary that was just replaced.
	for _, cmd := range [][]string{{"daemon-reload"}, {"enable", "blobstore.service"}, {"restart", "blobstore.service"}} {
		if err := systemctl(cmd...); err != nil {
			return err
		}
	}
	_ = systemctl("--no-pager", "--lines=0", "status", "blobstore.service")

	fmt.Println()
	fmt.Println("装好了。填进插件配置笔记里这个服务的 url 与 token(默认分组):")
	if conf, err := os.ReadFile(confPath); err == nil {
		for _, line := range strings.Split(string(conf), "\n") {
			if v, ok := strings.CutPrefix(line, "BLOBSTORE_PUBLIC_BASE="); ok {
				fmt.Println("  url    " + v + "/")
			}
			if v, ok := strings.CutPrefix(line, "BLOBSTORE_KEY="); ok {
				fmt.Println("  token  " + v)
			}
		}
	}
	fmt.Println("日志:journalctl -u blobstore -f")
	return nil
}

// Uninstall stops and removes the service and the binary. The data directory
// and the config stay, so a later install picks them up again.
func Uninstall() error {
	if err := preflight(); err != nil {
		return err
	}
	_ = systemctl("disable", "--now", "blobstore.service")
	for _, p := range []string{unitPath, binPath} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	_ = systemctl("daemon-reload")
	fmt.Printf("已卸载。数据目录 %s 与配置 %s 保留,要清除自行删除。\n", dataDir, confPath)
	return nil
}

func preflight() error {
	if runtime.GOOS != "linux" {
		return errors.New("install 只在 Linux 上用,要用 systemd")
	}
	if os.Geteuid() != 0 {
		return errors.New("要用 root 运行")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("这台机器没有 systemd")
	}
	return nil
}

// copySelf writes this executable to binPath by way of a temp file and a
// rename, which replaces the file even while the old binary is running.
func copySelf() error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(self); err == nil {
		self = resolved
	}
	if self == binPath {
		return nil
	}
	src, err := os.Open(self)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp := binPath + ".new"
	dst, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, binPath)
}

func writeConfig(addr, publicBase string) error {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	conf := fmt.Sprintf(configTemplate, addr, dataDir, hex.EncodeToString(buf), publicBase)
	return os.WriteFile(confPath, []byte(conf), 0o600)
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("systemctl %s 失败:%w", strings.Join(args, " "), err)
	}
	return nil
}

const configTemplate = `# blobstore 的配置,由 blobstore install 生成。改完执行 systemctl restart blobstore。

# 监听地址。挂在反向代理后面时用 127.0.0.1,只让代理连得上;不要改成 0.0.0.0,
# 那会把这个端口挂到这台机器的每一个网络接口上,包括公网的。
BLOBSTORE_ADDR=%s

# 存放目录。systemd 的 StateDirectory= 会创建它并设好属主,改到别处要自己建目录、
# 给服务的 uid 写权限,并把单元里的 StateDirectory= 去掉。
BLOBSTORE_ROOT=%s

# 默认分组的令牌:Authorization: Bearer <这个值>。填进 Trilium 里插件的配置笔记
# (带 #relocationConfig 的那篇)对应服务的 token。
BLOBSTORE_KEY=%s

# 写进返回 url 的地址前缀,不带结尾的斜杠。调用方与浏览器都要能打开它:挂在反向代理后面时
# 用对外的那个地址,含代理转发的路径前缀。
BLOBSTORE_PUBLIC_BASE=%s

# 其他分组,写成 名字:令牌,名字:令牌。每个分组有自己的目录与令牌,互相看不到对方的文件;
# 默认分组就是上面的 BLOBSTORE_KEY。留空则只有默认分组。
BLOBSTORE_TENANTS=

# 允许的浏览器来源,逗号分隔。请求由 Trilium 服务端发出,不带 Origin,这一项实际不起作用,留着备用。
BLOBSTORE_ORIGINS=
`
