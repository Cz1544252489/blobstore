// Command blobstore is a small file store for Trilium note content, in one
// binary with four roles: the storage service itself (no arguments, as
// systemd runs it), its installer (install, uninstall), the command-line
// client that moves notes over ETAPI (list, move, back, status, label), and
// the writer of the Trilium plugin's import zip (plugin). The service path
// calls only server.Run, so a running service never reads an ETAPI token or a
// Trilium database.
package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"

	"blobstore/internal/install"
	"blobstore/internal/relocate"
	"blobstore/internal/server"
	"blobstore/plugin"
)

//go:embed deploy/blobstore.service
var unitFile []byte

// version is bumped whenever behavior changes; -version prints it, and the plugin zip carries it.
var version = "v0.5.2"

func main() {
	if len(os.Args) < 2 || os.Args[1] == "serve" {
		exit(server.Run(version))
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch {
	case cmd == "-version" || cmd == "--version" || cmd == "version":
		fmt.Println("blobstore " + version)
	case cmd == "-h" || cmd == "--help" || cmd == "help":
		usage()
	case cmd == "install":
		exit(install.Install(args, unitFile))
	case cmd == "uninstall":
		exit(install.Uninstall())
	case cmd == "plugin":
		exit(writePlugin(args))
	case relocate.Commands[cmd]:
		exit(relocate.Run(cmd, args))
	default:
		fmt.Fprintf(os.Stderr, "未知命令:%s\n\n", cmd)
		usage()
		os.Exit(1)
	}
}

func writePlugin(args []string) error {
	fs := flag.NewFlagSet("plugin", flag.ContinueOnError)
	dir := fs.String("o", ".", "写到哪个目录")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path, err := plugin.Write(*dir, version)
	if err != nil {
		return err
	}
	fmt.Println(path)
	return nil
}

func usage() {
	fmt.Print(`blobstore —— Trilium 笔记内容的外部存放:服务、命令行与插件

  blobstore [serve]
      启动存放服务,配置取自 BLOBSTORE_* 环境变量(见 README)

  blobstore install [-addr <地址>] [-public-base <地址前缀>]
      (Linux,root)把这个程序装成 systemd 服务,或升级已装的;已有配置保留不动

  blobstore uninstall
      停止并删掉服务与程序,保留数据与配置

  blobstore plugin [-o <目录>]
      在目录里(默认当前目录)生成插件的导入 zip:blobstore-plugin-<版本>.zip

` + relocate.Usage + `
  blobstore -version
      打印版本号
`)
}

func exit(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "错误:"+err.Error())
		os.Exit(1)
	}
	os.Exit(0)
}
