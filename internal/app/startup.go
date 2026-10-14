package app

import "strings"

// StartupFailure is the user-facing explanation of a fatal startup error: the
// app could not open its index, or a migration refused to run. The CLI prints
// the same cause on stderr and exits non-zero; the desktop app shows this text
// in a window instead of disappearing without a trace.
type StartupFailure struct {
	Title         string
	Summary       string
	DatabaseLabel string
	Database      string
	ErrorLabel    string
	Error         string
	Guidance      string
	RevealLabel   string
	QuitLabel     string
}

// DescribeStartupFailure builds the message shown when the app cannot start. It
// is pure — language, database path and cause are all inputs — so the wording,
// the database path and the recovery guidance are unit-testable without a
// window or a running event loop.
//
// The guidance never tells the user to delete anything: the cause is usually a
// full disk, a permission problem or a leftover pre-migration backup, and the
// data next to the database is the only copy of their history.
func DescribeStartupFailure(lang, dbPath string, cause error) StartupFailure {
	detail := "unknown error"
	if cause != nil {
		if s := strings.TrimSpace(cause.Error()); s != "" {
			detail = s
		}
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "zh") {
		return StartupFailure{
			Title:         "MyToken 无法启动",
			Summary:       "启动时发生错误，应用在打开本地索引之前就停下了。历史数据没有被删除；按下面的提示排除原因后重新打开即可。",
			DatabaseLabel: "数据库位置",
			Database:      dbPath,
			ErrorLabel:    "错误详情",
			Error:         detail,
			Guidance: "请先退出所有 MyToken 进程（包括正在运行的命令行），确认磁盘空间充足，并检查上面的数据库文件及其 -wal、-shm 文件是否可读写。" +
				"如果仍然无法启动，请联系维护者，并在处理前保留 mytoken.db 与它旁边的 .pre-provider-v1.db、.pre-instance-v1.db 备份文件——不要删除、覆盖或重命名它们。" +
				"点击下面的按钮可以在访达或资源管理器中打开数据目录，便于查看这些文件。",
			RevealLabel: "打开数据目录",
			QuitLabel:   "退出",
		}
	}
	return StartupFailure{
		Title:         "MyToken could not start",
		Summary:       "A startup error stopped the app before it could open its local index. No history was deleted; fix the cause below and open the app again.",
		DatabaseLabel: "Database location",
		Database:      dbPath,
		ErrorLabel:    "Error details",
		Error:         detail,
		Guidance: "Quit every MyToken process first (including a running CLI command), make sure the disk has free space, and check that the database above and its -wal and -shm files are readable and writable. " +
			"If the app still will not start, contact the maintainer and keep mytoken.db together with any .pre-provider-v1.db / .pre-instance-v1.db backup files next to it — do not delete, overwrite or rename them. " +
			"The button below opens the data directory in your file manager so you can look at those files.",
		RevealLabel: "Open data folder",
		QuitLabel:   "Quit",
	}
}
