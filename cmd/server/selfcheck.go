// selfcheck.go 启动自检（v2.2 #13）：auth_dir / state 目录可写性 + 磁盘水位。
//
// 设计：**warn 而非 fatal**。容器/宿主挂载问题（卷只读、UID 不匹配、磁盘将满）
// 很常见，直接拒绝启动会让用户失去管理面板这个唯一的排查入口；改为启动时打
// WARN + 给出可操作的修复提示（chown / 检查挂载 / 清理磁盘），把决定权留给用户。
// 逻辑经 selfCheckDeps 注入 OS 依赖，纯函数可单测（不碰真实磁盘/权限）。
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// selfCheckMinFreeBytes 磁盘剩余低于该阈值（100 MiB）时 WARN：state.json 约 0.5-1 MB、
// 请求归档默认上限 100 MiB、用量/积分历史也要写盘，余量过小随时可能落盘失败。
const selfCheckMinFreeBytes = 100 << 20

// selfCheckDeps 启动自检的 OS 依赖（测试注入 fake，纯逻辑可确定性断言）。
type selfCheckDeps struct {
	Stat          func(path string) (os.FileInfo, error)
	MkdirAll      func(path string, perm os.FileMode) error
	ProbeWritable func(dir string) error
	FreeBytes     func(path string) (uint64, error)
}

// defaultSelfCheckDeps 生产依赖：真实文件系统 + 平台磁盘剩余空间实现。
func defaultSelfCheckDeps() selfCheckDeps {
	return selfCheckDeps{
		Stat:          os.Stat,
		MkdirAll:      os.MkdirAll,
		ProbeWritable: probeDirWritable,
		FreeBytes:     diskFreeBytes,
	}
}

// probeDirWritable 用「临时文件创建 + 删除」探测目录可写（无残留副作用）。
// 比单看权限位可靠：只读挂载/ACL/容器 UID 映射下 os.Stat 仍可能成功而写失败。
func probeDirWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".wb2a-write-test-*")
	if err != nil {
		return err
	}
	name := f.Name()
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(name)
		return cerr
	}
	return os.Remove(name)
}

// startupSelfCheck 用真实 OS 依赖执行启动自检，返回 WARN 文案列表（main 逐条打日志）。
// authDir/stateFile 来自配置；state 目录 = stateFile 的父目录。
func startupSelfCheck(authDir, stateFile string) []string {
	return selfCheck(authDir, stateFile, defaultSelfCheckDeps())
}

// selfCheck 启动自检纯逻辑（依赖可注入）。返回的每条都是可直接打日志的中文提示，
// 含「问题 + 后果 + 修复方向」；空列表 = 全部通过。
func selfCheck(authDir, stateFile string, deps selfCheckDeps) []string {
	var warns []string

	// 1) auth_dir：缺失/不可读/不是目录 → 账号扫描会得到 0 个（不阻断启动）。
	if st, err := deps.Stat(authDir); err != nil {
		if os.IsNotExist(err) {
			warns = append(warns, fmt.Sprintf(
				"auth_dir %q 不存在：将以 0 个账号启动；请确认目录/卷挂载（首次使用可 mkdir -p %s 后放入 workbuddy*.json）",
				authDir, authDir))
		} else {
			warns = append(warns, fmt.Sprintf(
				"auth_dir %q 不可访问: %v；容器部署请检查 volume 挂载与权限（Linux 可 chown -R <app-uid>:<app-uid> %s）",
				authDir, err, authDir))
		}
	} else if !st.IsDir() {
		warns = append(warns, fmt.Sprintf(
			"auth_dir %q 不是目录（当前是文件）：账号扫描（workbuddy*.json）会得到 0 个账号",
			authDir))
	}

	// 2) state 目录：不存在则尝试创建（正常首次运行路径）；创建失败/不可写 → WARN。
	stateDir := filepath.Dir(stateFile)
	if stateDir == "" || stateDir == "." {
		stateDir = "."
	}
	dirOK := true
	if st, err := deps.Stat(stateDir); err != nil {
		if os.IsNotExist(err) {
			if merr := deps.MkdirAll(stateDir, 0o755); merr != nil {
				dirOK = false
				warns = append(warns, fmt.Sprintf(
					"state 目录 %q 不存在且无法创建: %v；state.json 无法落盘（请检查挂载/权限：Docker 建议 chown -R <app-uid>:<app-uid> %s 或确认卷非只读）",
					stateDir, merr, stateDir))
			}
		} else {
			dirOK = false
			warns = append(warns, fmt.Sprintf(
				"state 目录 %q 不可访问: %v；请检查挂载/权限（Linux 可 chown -R <app-uid>:<app-uid> %s）",
				stateDir, err, stateDir))
		}
	} else if !st.IsDir() {
		dirOK = false
		warns = append(warns, fmt.Sprintf(
			"state 目录路径 %q 不是目录（当前是文件）：state.json 将无法落盘，重启后池状态丢失",
			stateDir))
	}
	if dirOK {
		if err := deps.ProbeWritable(stateDir); err != nil {
			warns = append(warns, fmt.Sprintf(
				"state 目录 %q 不可写: %v；state.json 无法落盘（容器请检查卷是否只读、运行用户权限；Linux 可 chown -R <app-uid>:<app-uid> %s）",
				stateDir, err, stateDir))
		}
	}

	// 3) 磁盘水位：以 state 目录所在卷为准（目录不存在时向上找存在祖先）。
	probeDir := existingAncestor(stateDir, deps.Stat)
	free, err := deps.FreeBytes(probeDir)
	if err != nil {
		warns = append(warns, fmt.Sprintf(
			"无法读取 %q 所在磁盘剩余空间（跳过水位自检）: %v", probeDir, err))
	} else if free < selfCheckMinFreeBytes {
		warns = append(warns, fmt.Sprintf(
			"磁盘剩余 %d MiB 低于 %d MiB 阈值：state.json / 请求归档 / 用量数据可能写失败，请清理或扩容（路径 %q）",
			free>>20, selfCheckMinFreeBytes>>20, probeDir))
	}
	return warns
}

// existingAncestor 返回 path 或其最近的已存在祖先目录（用于磁盘水位探测）。
// 依赖注入的 Stat 便于测试构造「目录还不存在」的场景。
func existingAncestor(path string, stat func(string) (os.FileInfo, error)) string {
	dir := path
	for {
		if _, err := stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir || parent == "" {
			return "."
		}
		dir = parent
	}
}
