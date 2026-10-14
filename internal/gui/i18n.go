package gui

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/zzstar101/mytoken/internal/model"
)

// Lang is "zh" or "en".
var (
	langOnce sync.Once
	lang     = ""
)

// SetLang forces the interface language ("zh" or "en"); tests use it.
func SetLang(l string) { langOnce.Do(func() {}); lang = l }

// Lang returns the interface language, following the system's.
func Lang() string {
	langOnce.Do(func() { lang = detectLang() })
	return lang
}

func detectLang() string {
	for _, k := range []string{"MYTOKEN_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" && v != "c" && v != "posix" {
			if strings.HasPrefix(v, "zh") {
				return "zh"
			}
			if k == "MYTOKEN_LANG" {
				return "en"
			}
			break
		}
	}
	if runtime.GOOS == "darwin" {
		// GUI apps get no LANG; ask the user defaults.
		if out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output(); err == nil {
			s := strings.ToLower(string(out))
			if i := strings.IndexAny(s, "\"abcdefghijklmnopqrstuvwxyz"); i >= 0 {
				s = strings.TrimLeft(s[i:], "\"")
				if strings.HasPrefix(s, "zh") {
					return "zh"
				}
			}
		}
	}
	return "en"
}

var strs = map[string][2]string{ // key: {zh, en}
	"overview":         {"概览", "Overview"},
	"sessions":         {"会话", "Sessions"},
	"ranking":          {"排行", "Ranking"},
	"projects":         {"项目", "Projects"},
	"settings":         {"设置", "Settings"},
	"today":            {"今天", "Today"},
	"7d":               {"7 天", "7 days"},
	"30d":              {"30 天", "30 days"},
	"90d":              {"90 天", "90 days"},
	"all":              {"全部", "All"},
	"tokens":           {"Tokens", "Tokens"},
	"cost":             {"费用", "Cost"},
	"requests":         {"请求", "Requests"},
	"cacheHit":         {"缓存命中", "Cache hit"},
	"input":            {"输入", "Input"},
	"output":           {"输出", "Output"},
	"cacheRead":        {"缓存读", "Cache read"},
	"cacheWrite":       {"缓存写", "Cache write"},
	"reasoning":        {"推理", "Reasoning"},
	"trend":            {"每日用量", "Daily usage"},
	"composition":      {"Token 构成", "Token mix"},
	"activity":         {"活跃热力", "Activity"},
	"topModels":        {"常用模型", "Top models"},
	"byHarness":        {"按工具", "By harness"},
	"byProvider":       {"供应商", "Providers"},
	"byModel":          {"模型", "Models"},
	"recent":           {"最近", "Recent"},
	"mostTokens":       {"最多 Token", "Most tokens"},
	"mostCost":         {"最贵", "Costliest"},
	"failed":           {"出错了", "Failed"},
	"mapped":           {"已把 %s 映射为 %s", "Mapped %s to %s"},
	"badMultiplier":    {"倍率要是一个大于 0 的数字", "The multiplier must be a number above 0"},
	"multiplierSet":    {"%s 的倍率已设为 %s×", "%s now bills at %s×"},
	"unpricedTitle":    {"没找到价格的模型", "Models without a price"},
	"unpricedSub":      {"这些名字在价格表里找不到，多半是中转站或配置里自定义的别名。映射到真实模型后会按它计价，并在排行里合并。", "These names aren't in the price list — usually aliases a relay or your config made up. Map each to the real model to price it and merge it in rankings."},
	"allPriced":        {"所有模型都找到价格了 ✓", "Every model has a price ✓"},
	"importCCSwitch":   {"导入 cc-switch 价格", "Import cc-switch prices"},
	"imported":         {"从 cc-switch 导入了 %d 条价格规则", "Imported %d price rules from cc-switch"},
	"aliasesTitle":     {"模型映射", "Model mappings"},
	"remove":           {"移除", "Remove"},
	"multipliersTitle": {"供应商倍率", "Provider multipliers"},
	"multipliersSub":   {"中转站常按官方价的若干倍计费；填好后点“应用”生效，数字按你输入的原样保存，不会被四舍五入。日志自带的费用不受影响。", "Relays often bill a multiple of list price; press Apply to save. The number is stored exactly as typed, never rounded to a step. Costs the log reported stay as they are."},
	"multiplierLabel":  {"%s 的倍率", "%s multiplier"},
	"multiplierApply":  {"应用 %s 的倍率", "Apply %s multiplier"},
	"apply":            {"应用", "Apply"},
	"customPrices":     {"另有 %d 条模型自定义价格（来自 cc-switch 或命令行）", "Plus %d custom model prices (from cc-switch or the CLI)"},
	"nReq":             {"%s 次请求", "%s requests"},
	"mapTo":            {"映射为", "Map to"},
	"other":            {"其他…", "Other…"},
	"cancel":           {"取消", "Cancel"},
	"searchCatalog":    {"搜索价格表里的模型", "Search the price list"},
	"noMatch":          {"没有匹配的模型", "No matching model"},
	"unpricedBanner":   {"%d 个模型没找到价格，费用可能偏低", "%d models have no price, so costs may be low"},
	"fixIt":            {"去映射 →", "Map them →"},
	"noModelName":      {"（日志里没有模型名）", "(no model name in the log)"},
	"noModelNameSub":   {"这些请求没有记录模型，无法映射；多半是中转站的探活或报错请求。", "These requests never named a model, so there's nothing to map — usually relay health checks or errors."},
	"search":           {"搜索会话、项目、模型…", "Search sessions, projects, models…"},
	"noSessions":       {"还没有会话呢", "No sessions yet"},
	"noSessionsSub":    {"迷子でもいい、前へ進め——先去写点代码吧", "Go write some code, the tokens will follow"},
	"pickSession":      {"选一个会话看看", "Pick a session"},
	"pickSub":          {"左边的每一行都是一次和 AI 的合奏", "Every row on the left is a jam session with an AI"},
	"subagents":        {"子代理", "Subagents"},
	"breakdown":        {"供应商 × 模型", "Provider × model"},
	"timeline":         {"请求时间线", "Request timeline"},
	"started":          {"开始", "Started"},
	"updated":          {"更新", "Updated"},
	"project":          {"项目", "Project"},
	"inferred":         {"推断", "inferred"},
	"viaCCSwitch":      {"cc-switch", "cc-switch"},
	"viaConfig":        {"配置", "config"},
	"viaRule":          {"规则", "rule"},
	"viaLog":           {"日志", "log"},
	"scanning":         {"正在扫描", "Scanning"},
	"upToDate":         {"数据已是最新", "Data up to date"},
	"autoUpdate":       {"自动检查更新", "Check for updates automatically"},
	"autoUpdateSub":    {"每天访问一次 github.com，看有没有新版本", "Asks github.com once a day whether there is a new version"},
	"noSelfUpdate":     {"这个版本不能自己更新（开发版，或由 apt 等包管理器安装）", "This build can't update itself (a development build, or installed by a package manager)"},
	"checkUpdates":     {"检查更新", "Check for Updates"},
	"currentVersion":   {"当前版本 %s", "Version %s"},
	"openMain":         {"打开主窗口", "Open MyToken"},
	"quit":             {"退出", "Quit"},
	"last24h":          {"最近 24 小时", "Last 24 hours"},
	"activeNow":        {"正在进行", "Active now"},
	"nothingToday":     {"今天还很安静", "Quiet today"},
	"nothingTodaySub":  {"一辈子……的 token，从第一句开始", "Every token starts with a first prompt"},
	"launchAtLogin":    {"开机自动启动", "Open at login"},
	"launchAtLoginSub": {"在菜单栏常驻，随时看一眼", "Live in the menu bar"},
	"rebuild":          {"重建索引", "Rebuild index"},
	"rebuildSub":       {"重新扫描全部日志（历史永久保留在本地数据库中）", "Rescan every log (history stays in the local database)"},
	"dataDir":          {"数据目录", "Data folder"},
	"sources":          {"数据来源", "Sources"},
	"sourceRequests":   {"%s 次请求", "%s requests"},
	"sourcesIdle":      {"另外 %d 个支持的工具没有发现数据", "%d other supported tools have no data here"},
	"sourcesSub":       {"只读本地日志，从不上传；仅价格表会联网更新", "Reads local logs only; only the price list goes online"},
	"about":            {"关于", "About"},
	"sessionsN":        {"%d 个会话", "%d sessions"},
	"requestsN":        {"%s 次请求", "%s requests"},
	"childrenN":        {"%d 个子代理", "%d subagents"},
	"share":            {"占比", "Share"},
	"lessThanMin":      {"刚刚", "just now"},
	"minAgo":           {"%d 分钟前", "%dm ago"},
	"hourAgo":          {"%d 小时前", "%dh ago"},
	"dayAgo":           {"%d 天前", "%dd ago"},
	"untitled":         {"（无标题）", "(untitled)"},
	"vsPrev":           {"较上期", "vs prev."},
	"perDay":           {"日均", "per day"},
	"less":             {"少", "Less"},
	"more":             {"多", "More"},
	"m2":               {"这一页会在下个里程碑完成", "Coming in the next milestone"},
	"busiestDay":       {"最忙的一天", "Busiest day"},
	"streak":           {"连续活跃", "Streak"},
	"days":             {"%d 天", "%d days"},
	"noData":           {"暂无数据", "No data"},
	"noDataSub":        {"这段时间舞台上很安静", "The stage has been quiet for this span"},
	"noResults":        {"什么都没找到", "Nothing found"},
	"noResultsSub":     {"拨片掉了一地，也没找到它——换个关键词试试", "Picks everywhere, but not that one — try another word"},
	"allHarnesses":     {"全部工具", "All harnesses"},
	"heavy":            {"为什么要烧这么多 token！", "Why burn so many tokens?!"},
	"span":             {"时间范围", "Time range"},
	"sortBy":           {"排序", "Sort by"},
	"rankingBy":        {"排行依据", "Rank by"},
	"instance":         {"实例", "Instance"},
	"providerSessions": {"查看会话", "View sessions"},
	"providerSessionsSub": {
		"只看这个端点服务过的会话", "Only the sessions this endpoint served",
	},
	"providerSessionsFilter": {"按端点筛选", "Filtered by endpoint"},
	"providerSessionsClear":  {"清除", "Clear"},

	// Budget: the rule list, the editor and the delivery report.
	"budgetTitle":    {"预算提醒", "Budget alerts"},
	"budgetAdd":      {"新建规则", "New rule"},
	"budgetEdit":     {"编辑", "Edit"},
	"budgetSave":     {"保存", "Save"},
	"budgetNone":     {"还没有预算规则", "No budget rules yet"},
	"budgetNoneSub":  {"按本地估算费用在日/周/月内提醒", "Alert on local estimated cost per day, week or month"},
	"budgetPaused":   {"已暂停", "paused"},
	"budgetName":     {"名称", "Name"},
	"budgetNameSub":  {"通知里显示的名字，可留空", "Shown in the notification; may be empty"},
	"budgetScope":    {"范围", "Scope"},
	"budgetScopeSub": {"全局、项目或端点", "Total, a project or an endpoint"},
	"budgetScopeID":  {"稳定标识", "Stable id"},
	"budgetScopeIDSub": {
		"项目路径或端点标识，与用量页一致",
		"The project path or endpoint identity shown on the usage pages",
	},
	"budgetPeriod":     {"周期", "Period"},
	"budgetPeriodSub":  {"按统计时区划分，周一为周首", "Resolved in the stats timezone, weeks start Monday"},
	"budgetLimit":      {"限额", "Limit"},
	"budgetLimitSub":   {"本期本地估算的上限", "Cap on this period's estimated local cost"},
	"budgetThresholds": {"阈值", "Thresholds"},
	"budgetThresholdsSub": {
		"逗号分隔的百分比；留空用 80,100",
		"Percentages, comma separated; empty uses 80,100",
	},
	"budgetCurrency": {"币种", "Currency"},
	"budgetCurrencySub": {
		"三位代码；不同币种不换算也不合并",
		"Three-letter code; currencies are never converted or combined",
	},
	"budgetEnabled": {"启用", "Enabled"},
	"budgetSaved":   {"已保存", "Saved"},
	"budgetDeleted": {"已删除", "Deleted"},
	"budgetSource": {
		"只按本地估算费用提醒，不含日志或中转的账单金额；没有已知价格的请求不计入，并会明确标出。",
		"Alerts use local estimated cost only, never log or relay billed amounts; requests with no known price are excluded and called out.",
	},
	"budgetOnce": {
		"每个周期每个阈值至多通知一次：先把预留写进存储，再发出系统通知；系统拒绝不会重发。",
		"At most one notification per period and threshold: the reservation is written to the store before the system notification, and a refused delivery is not retried.",
	},
	"budgetNever":   {"还没检查过", "Not checked yet"},
	"budgetCheck":   {"上次检查", "Last check"},
	"budgetNotify":  {"通知已开启", "Notifications on"},
	"budgetNoNotif": {"未安装通知器", "No notifier installed"},
	"budgetEstimate": {
		"估算 %s%.2f / %s%.2f（%.0f%%）",
		"Estimated %s%.2f of %s%.2f (%.0f%%)",
	},
	"budgetUnknown": {
		"· %d 次请求没有已知价格，估算偏低",
		"· %d requests have no known price, so this is a lower bound",
	},
	"budgetLowerBound": {
		"· 含无已知价格的请求，实为下限",
		"· includes unpriced requests, so this is a lower bound",
	},
	"budgetNoData":        {"本期还没有数据", "No data for this period yet"},
	"budgetFail":          {"投递失败", "Delivery failed"},
	"budgetSent":          {"已通知", "Delivered"},
	"budgetScopeGlobal":   {"全局", "Total"},
	"budgetScopeProject":  {"项目", "Project"},
	"budgetScopeProvider": {"端点", "Endpoint"},
	"budgetTz":            {"统计时区", "Stats timezone"},
	"budgetTzSub": {
		"所有日历周期按此时区划分，需要 IANA 名称，如 Asia/Shanghai",
		"Every calendar period uses this zone; choose an IANA name such as Asia/Shanghai",
	},
	"budgetTzApply": {"应用", "Apply"},
	"budgetTzSaved": {"时区已保存", "Timezone saved"},
	"budgetTzBad":   {"时区名无效", "Unknown timezone"},
	"budgetTzUnset": {"未设置", "not set"},
	"budgetTzNeeds": {
		"尚未设定固定统计时区，请选择 IANA 时区；在此之前预算不会发送通知。",
		"No fixed stats timezone is set; choose an IANA zone. Until you do, budget checks send no notifications.",
	},
	"budgetTzPreview": {"当前系统时区（仅供参考，未采用）：%s", "System zone now (preview only, not used): %s"},
	"budgetTzErr": {
		"存储的时区无法使用，需要重新选择；在此之前预算不会发送通知",
		"The stored timezone cannot be used; choose one. Until then budget checks send no notifications",
	},
	"sourcesInstances": {"数据来源（实例）", "Sources (instances)"},
	"sourcesIndepSub": {
		"每个条目都是一个独立实例：各自扫描、各自计数，内容相同也不会合并。备份副本只保留配置和诊断，永不被扫描。保存后需要重启才生效。",
		"Each entry is an independent instance: scanned and counted on its own, and equal content is never merged. A backup copy keeps its configuration and diagnostics but is never scanned. Saved changes take effect after a restart.",
	},
	"srcAdd":       {"添加", "Add"},
	"srcAddTitle":  {"添加数据来源", "Add a source"},
	"srcMoveTitle": {"移动实例 %s", "Move instance %s"},
	"srcHarness":   {"工具", "Harness"},
	"srcLabel":     {"名称（可选）", "Label (optional)"},
	"srcRoots":     {"目录", "Roots"},
	"srcRootsHint": {
		"多个目录用逗号、分号或换行分隔；路径必须存在且可读，否则保存会被拒绝。",
		"Separate multiple roots with a comma, semicolon or newline; every path must exist and be readable or the save is refused.",
	},
	"srcAddKindNote": {
		"关闭「备份副本」即创建一个独立实例；备份副本永远不会被扫描或计入统计。",
		"Leave the backup copy off to create an independent instance; a backup copy is never scanned and never counted.",
	},
	"srcSave":           {"保存", "Save"},
	"srcMove":           {"移动", "Move"},
	"srcPromote":        {"转为独立实例", "Promote to instance"},
	"srcConfirmPromote": {"确认转为独立实例", "Confirm promote"},
	"srcPromoteNote": {
		"转为独立实例后，该目录的内容将开始被扫描并计入统计。ID 保持不变，磁盘内容不会被改动。",
		"Promoting starts scanning and counting this directory. The ID is unchanged and nothing on disk is touched.",
	},
	"srcConfirmRemove": {"确认移除", "Confirm remove"},
	"srcRemoveNote": {
		"只删除 sources.json 中的这一条配置；磁盘上的日志不会被删除。",
		"Removes only this entry from sources.json; nothing on disk is deleted.",
	},
	"srcAdded": {"已添加并开始扫描", "Added; scanning now"},
	"srcMoved": {
		"已移动，ID 与历史保持不变，已开始扫描新位置",
		"Moved; the ID and history are unchanged and the new path is being scanned",
	},
	"srcRemoved":  {"已移除配置", "Entry removed"},
	"srcPromoted": {"已转为独立实例，已开始扫描", "Promoted; scanning now"},
	"srcRestart": {
		"配置已更改，但未能应用到正在运行的扫描：当前进程仍按上一次配置扫描。",
		"Configuration changed, but it was not applied to the running scan: this process still scans the previous configuration.",
	},
	"srcScanStopped": {
		"扫描已停止：%s",
		"Scanning stopped: %s",
	},
	"srcRunning":    {"当前正在运行：%d/%d 个实例被扫描", "Running now: %d of %d instances are scanned"},
	"srcWorking":    {"处理中…", "Working…"},
	"srcNone":       {"没有配置任何数据来源", "No source is configured"},
	"srcUser":       {"配置", "configured"},
	"srcEnv":        {"环境变量", "env"},
	"srcDefault":    {"默认", "default"},
	"srcBackupCopy": {"备份副本", "Backup copy"},
	"srcBackupTip": {
		"只保留配置和诊断，不扫描、不计数",
		"Kept for configuration and diagnostics only: not scanned, not counted",
	},
	"srcDisabled":           {"已停用", "Disabled"},
	"srcNoRoots":            {"没有可用目录", "No usable root"},
	"srcNotScanned":         {"未被扫描", "Not scanned"},
	"srcNotScannedBackup":   {"未被扫描（备份副本）", "Not scanned (backup copy)"},
	"srcNotScannedDisabled": {"未被扫描（已停用）", "Not scanned (disabled)"},
	"srcScanned":            {"正在扫描", "Scanned"},
	"srcFatal":              {"致命", "fatal"},
	"srcHintOther": {
		"检查该目录是否存在、可读，且不与同一工具的其他目录重叠。",
		"Check that the directory exists, is readable, and does not overlap another root of the same harness.",
	},
	"srcHint_missing-dir":     {"目录不存在", "The directory does not exist"},
	"srcHint_not-dir":         {"路径不是目录", "The path is not a directory"},
	"srcHint_permission":      {"没有读取权限", "The directory is not readable"},
	"srcHint_invalid-root":    {"目录无效", "Invalid root"},
	"srcHint_duplicate-root":  {"同一实例重复列出了该目录", "This instance lists the directory twice"},
	"srcHint_overlap":         {"与同一工具的其他目录重叠", "Overlaps another root of the same harness"},
	"srcHint_no-roots":        {"启用的实例至少需要一个目录", "An enabled instance needs at least one root"},
	"srcHint_unknown-harness": {"无法识别的工具", "Unknown harness"},
	"srcHint_invalid-kind":    {"条目类型无效", "Invalid entry kind"},
	"srcHint_duplicate-id":    {"实例 ID 重复", "Duplicate instance ID"},
	"srcHint_invalid-config":  {"sources.json 格式无效", "sources.json is malformed"},
	"srcHint_unsupported-version": {
		"sources.json 版本过新，请升级 MyToken",
		"sources.json is from a newer version; update MyToken",
	},
	"srcHint_not-found":   {"找不到该实例", "No such instance"},
	"srcHint_io-error":    {"读写配置失败", "Could not read or write the configuration"},
	"srcHint_reserved-id": {"该 ID 为保留值", "That ID is reserved"},
	"srcHint_no-factory":  {"该工具没有可用的解析器", "No parser is registered for this harness"},
	// The sources card's collection evidence, as the scanner recorded it. A
	// record that is missing says so, and one written before a move is
	// labelled last known: neither is ever shown as a scan of zero.
	"srcDiagAttempt":    {"上次尝试：%s", "last attempt: %s"},
	"srcDiagSuccess":    {"上次成功：%s", "last success: %s"},
	"srcDiagNever":      {"从未", "never"},
	"srcDiagFiles":      {"识别文件：%s", "recognized files: %s"},
	"srcDiagEvents":     {"新增事件：%s", "new events: %s"},
	"srcDiagSessions":   {"已索引会话：%s", "indexed sessions: %s"},
	"srcDiagReason":     {"原因：%s", "reason: %s"},
	"srcDiagTruncated":  {"记录只含前 %d 个来源", "record holds only its first %d sources"},
	"srcDiagNone":       {"尚无采集记录：该实例还没有完成过一次采集", "No collection evidence yet: no scan of this instance has completed"},
	"srcDiagHistorical": {"以上是移动前位置的最后已知记录，尚未按当前目录完成新的采集", "Last known from the previous location: no scan of the current roots has completed yet"},
	"claudeQuota":       {"Claude 配额", "Claude quota"},
	"claudeQuotaSub": {
		"默认关闭。开启前请确认下面的凭据来源和请求目标；关闭后立即停止自动请求。",
		"Off by default. Before enabling, check the credential source and request target below; disabling stops automatic requests immediately.",
	},
	"claudeQuotaDisclose": {"开启前请确认", "Before enabling"},
	"claudeQuotaTarget":   {"请求目标", "Request target"},
	"claudeQuotaRef":      {"凭据来源", "Credential source"},
	"claudeQuotaScope": {
		"开启后，MyToken 只读取该凭据用于查询用量；凭据不会写入数据库，也不会出现在日志里。",
		"Once enabled, MyToken reads this credential only to query usage; the credential is never written to the database or the logs.",
	},
	"claudeQuotaGranted":  {"授权时间", "Granted"},
	"claudeQuotaPick":     {"请先选择一个凭据来源", "Pick a credential source first"},
	"claudeQuotaPickHint": {"选择一个来源（不会自动选择）：", "Pick a source (nothing is preselected):"},
	"claudeQuotaEnable":   {"开启", "Enable"},
	"claudeQuotaEnabled":  {"已开启", "Enabled"},
	"claudeQuotaDisabled": {"已关闭", "Off"},
	"claudeQuotaNoConsent": {
		"尚未开启：没有记录任何授权，不会读取任何凭据来源，也不会发起请求。",
		"Not enabled: no consent is recorded, no credential source is read and no request is made.",
	},
	"claudeQuotaNoSnapshot": {
		"已开启，但读不到本地缓存的最新记录；下面是最后一次记录的状态。",
		"Enabled, but the cached reading could not be read; the last recorded state is shown below.",
	},
	"claudeQuotaEnabledMsg":  {"已开启，正在查询…", "Enabled; querying…"},
	"claudeQuotaDisabledMsg": {"已关闭，自动请求已停止", "Disabled; automatic requests stopped"},
	"claudeQuotaRefreshed":   {"已刷新", "Refreshed"},
	"claudeQuotaCached": {
		"显示的是上次缓存的结果；只有点「刷新」才会发起请求。",
		"Showing the cached reading; a request is made only when you press Refresh.",
	},
	"claudeQuotaRefresh":     {"刷新", "Refresh"},
	"claudeQuotaDisable":     {"关闭", "Disable"},
	"claudeQuotaChosen":      {"已选择", "Selected"},
	"claudeQuotaKeychain":    {"钥匙串", "keychain"},
	"claudeQuotaFile":        {"凭据文件", "credentials file"},
	"claudeQuotaUnsupported": {"不可用", "unavailable"},
	"claudeQuotaFileMissing": {"文件不存在", "file not found"},
	"claudeQuotaNoSources": {
		"没有找到可用的凭据来源；不会回退到默认位置。",
		"No usable credential source was found; there is no fallback to the default location.",
	},
	"claudeQuotaNever":                     {"尚未获取", "never fetched"},
	"claudeQuotaFetched":                   {"%s前获取", "fetched %s ago"},
	"claudeQuotaReason":                    {"原因", "Reason"},
	"claudeQuotaOK":                        {"有效", "ok"},
	"claudeQuotaStale":                     {"已过期", "stale"},
	"claudeQuotaUnavailable":               {"不可用", "unavailable"},
	"claudeQuotaNoWindows":                 {"响应中没有窗口数据", "The response carried no windows"},
	"claudeQuotaUsed":                      {"已用 %s", "%s used"},
	"claudeQuotaRemaining":                 {"剩余 %s", "%s left"},
	"claudeQuotaMinutes":                   {"%d 分钟窗口", "%d-minute window"},
	"claudeQuotaResets":                    {"%s 重置", "resets %s"},
	"claudeQuotaFiveHour":                  {"5 小时", "5-hour"},
	"claudeQuotaSevenDay":                  {"7 天", "7-day"},
	"claudeQuotaSevenDayOpus":              {"7 天 Opus", "7-day Opus"},
	"claudeQuotaSevenDaySonnet":            {"7 天 Sonnet", "7-day Sonnet"},
	"claudeQuotaReasonOther":               {"其他原因", "other reason"},
	"claudeQuotaReason_not_enabled":        {"未开启", "not enabled"},
	"claudeQuotaReason_consent_invalid":    {"授权已失效，需要重新开启", "the consent no longer matches; enable again"},
	"claudeQuotaReason_offline":            {"离线模式，未发起请求", "offline; no request was made"},
	"claudeQuotaReason_disabled":           {"已关闭", "disabled"},
	"claudeQuotaReason_throttled":          {"请求过于频繁，稍后自动重试", "throttled; will retry later"},
	"claudeQuotaReason_no_source":          {"没有可用的凭据来源", "no usable credential source"},
	"claudeQuotaReason_store_unavailable":  {"本地存储不可用", "the local store is unavailable"},
	"claudeQuotaReason_source_missing":     {"凭据来源不存在", "the credential source is missing"},
	"claudeQuotaReason_source_unreadable":  {"凭据无法读取", "the credential cannot be read"},
	"claudeQuotaReason_source_unsupported": {"该来源不支持读取", "this source cannot be read"},
	"claudeQuotaReason_token_missing":      {"凭据中没有令牌", "the credential has no token"},
	"claudeQuotaReason_token_expired":      {"令牌已过期", "the token has expired"},
	"claudeQuotaReason_unauthorized":       {"凭据被拒绝", "the credential was rejected"},
	"claudeQuotaReason_rate_limited":       {"服务端限流", "rate limited by the service"},
	"claudeQuotaReason_redirect_refused":   {"拒绝了重定向", "a redirect was refused"},
	"claudeQuotaReason_http_status":        {"服务端返回错误状态", "the service returned an error status"},
	"claudeQuotaReason_network":            {"网络请求失败", "the network request failed"},
	"claudeQuotaReason_canceled":           {"请求已取消", "the request was canceled"},
	"claudeQuotaReason_malformed_response": {"响应无法解析", "the response could not be parsed"},
	"claudeQuotaReason_no_windows":         {"响应中没有窗口数据", "the response carried no windows"},
	"quotaOffline":                         {"Codex 配额（本地记录）", "Codex quota (local record)"},
	"quotaOfflineSub": {
		"读取本地日志中记录的用量上限，按实例区分；只显示记录到的事实，不显示账户总额，也不推算消耗速度。",
		"Reads the limits recorded in the local logs, per instance. It shows only what was recorded: no account totals and no inferred pace.",
	},
	"quotaNoData":      {"本地日志中没有该实例的配额记录", "The local logs hold no quota record for this instance"},
	"quotaNoDataShort": {"无记录", "no record"},
	"quotaNoDataHint": {
		"需要先扫描到包含配额信息的日志行。",
		"A log line carrying quota information must be scanned first.",
	},
	"quotaCorrupt":            {"存储的配额记录已损坏，无法显示", "The stored quota record is corrupt and cannot be shown"},
	"quotaCorruptShort":       {"记录损坏", "corrupt"},
	"quotaObserved":           {"记录时间：%s", "Observed %s"},
	"quotaIngested":           {"入库时间：%s", "Stored %s"},
	"quotaNoTimestamp":        {"记录没有可信的时间戳", "The record has no trustworthy timestamp"},
	"quotaSource":             {"来源", "Source"},
	"quotaReason":             {"原因", "Reason"},
	"quotaPrimary":            {"主要窗口", "Primary window"},
	"quotaSecondary":          {"次要窗口", "Secondary window"},
	"quotaUsedPct":            {"已用 %s", "%s used"},
	"quotaWindowMinutes":      {"窗口 %d 分钟", "%d-minute window"},
	"quotaResetsAt":           {"%s 重置", "resets %s"},
	"quotaResetsIn":           {"%s 后重置", "resets in %s"},
	"quotaRemainingDerived":   {"剩余量由已用量推算", "remaining derived from used"},
	"quotaCredits":            {"额度", "Credits"},
	"quotaCreditsHas":         {"有可用额度", "has credits"},
	"quotaCreditsNone":        {"没有可用额度", "no credits"},
	"quotaCreditsUnlimited":   {"无限", "unlimited"},
	"quotaBalance":            {"余额 %s", "balance %s"},
	"quotaUnitsUnstated":      {"单位未标明", "unit not stated"},
	"quotaNoTotals":           {"不显示账户总额：窗口用量以百分比表示。", "No account totals are shown: window usage is a percentage."},
	"quotaIssues":             {"%d 条记录问题", "%d recorded issues"},
	"quotaTruncated":          {"日志被截断，结果可能不完整", "The log was truncated; the result may be incomplete"},
	"quotaOK":                 {"有效", "ok"},
	"quotaStale":              {"已过期", "stale"},
	"quotaUnknown":            {"未知", "unknown"},
	"quotaMalformed":          {"格式错误", "malformed"},
	"quotaUnavailable":        {"不可用", "unavailable"},
	"quotaReasonNoData":       {"没有数据", "no data"},
	"quotaReasonMissing":      {"记录缺失", "the record is missing"},
	"quotaReasonMalformed":    {"记录格式错误", "the record is malformed"},
	"quotaReasonStale":        {"记录已过期", "the record is stale"},
	"quotaReasonNoTimestamp":  {"没有可信时间戳", "no trustworthy timestamp"},
	"quotaReasonNewerInvalid": {"更新的记录无效，保留上一条", "a newer record was invalid; the previous one is kept"},
	"quotaReasonReset":        {"重置时间无法确定", "the reset time cannot be determined"},
	"quotaReasonOther":        {"其他原因", "other reason"},
	"quotaReasonWindowMismatch": {
		"两次样本来自不同的窗口", "the two samples are of different windows",
	},
	"quotaReasonLimitMismatch": {
		"两次样本来自不同的限额桶", "the two samples are of different limit buckets",
	},
	"quotaReasonZeroElapsed": {
		"两次样本的间隔太短或为零", "the interval between the two samples is too short",
	},
	"quotaReasonDecreasing": {
		"用量没有上升，无法推算速度", "usage did not rise, so no pace can be derived",
	},
	"quotaReasonOutlier": {
		"推算出的速度超出可信范围", "the derived rate is beyond a plausible range",
	},
	"quotaReasonNoObservation": {
		"没有该窗口的观测记录", "there is no observation of this window",
	},
	"quotaReasonInsufficient": {
		"样本不足（只有一个观测点，一个点不是速度）", "not enough samples (one observation is not a rate)",
	},
	"quotaReasonFuture": {
		"观测时间在将来", "the observation is timestamped in the future",
	},
	"quotaReasonSourceMismatch": {
		"两次样本来自不同的来源", "the two samples come from different sources",
	},
	"quotaPaceTitle":   {"消耗速度推算", "Pace forecast"},
	"quotaPaceFrom":    {"基于最新观测（%s前，%s）", "from the newest observation (%s ago, %s)"},
	"quotaPaceSamples": {"最多 %d 个样本", "up to %d samples"},
	"quotaPaceRate":    {"实测 %s（用时 %s）", "measured %s (over %s)"},
	"quotaPaceIdle": {
		"此区间没有消耗，因此没有耗尽时间。", "nothing was consumed in this interval, so there is no exhaustion time",
	},
	"quotaPaceResetsFirst": {
		"窗口会先重置，耗尽时间不可操作。", "the window resets first, so the exhaustion time is not actionable",
	},
	"quotaPaceExhausts": {"按此速度 %s 后耗尽（%s）", "exhausts in %s at this pace (%s)"},
	"quotaPaceEven": {
		"均速参考 %s（重置前刚好用完）", "even-use reference %s (would exhaust exactly at the reset)",
	},
	"quotaPaceUnavailable":   {"无法推算速度：%s", "no pace: %s"},
	"quotaPaceNotActionable": {"不据此行动：%s", "not actionable: %s"},
	"quotaLimitBursty": {
		"速度按最近两个样本的直线推算，不模拟突发用量",
		"pace is a straight line through the two most recent samples, so bursty usage is not modelled",
	},
	"quotaLimitReset": {
		"窗口重置会丢弃样本对：新周期的样本到达前不给出推算",
		"a window reset discards the sample pair, so no forecast is stated until a sample from the new period arrives",
	},
	"quotaLimitExhaust": {
		"只有用量上升且耗尽点早于重置时才给出耗尽时间，否则只报告重置",
		"exhaustion is projected only while usage rises and the projection lands before the reset; otherwise the reset is reported instead",
	},
	"ingestRate": {"入库速率", "Ingest rate"},
	"ingestRateTip": {
		"最近 60 秒已提交的输出 token 速率，不是模型生成速度",
		"Committed output tokens per second over the last 60 seconds; not model generation speed",
	},
	"ingestRateUnknown": {"未知", "unknown"},
	"ingestRateWindow":  {"%d 秒窗口", "%d-second window"},
}

// tr translates key.
func tr(key string) string {
	s, ok := strs[key]
	if !ok {
		return key
	}
	if Lang() == "zh" {
		return s[0]
	}
	return s[1]
}

func trf(key string, args ...any) string { return fmt.Sprintf(tr(key), args...) }

// fmtTokens shortens a token count: 1.23亿 / 4,567万 in Chinese, 1.23B / 45.6M in English.
func fmtTokens(n int64) string {
	f := float64(n)
	if Lang() == "zh" {
		switch {
		case f >= 1e8:
			return trimNum(f/1e8, 2) + "亿"
		case f >= 1e4:
			return trimNum(f/1e4, 1) + "万"
		}
		return fmtInt(n)
	}
	switch {
	case f >= 1e9:
		return trimNum(f/1e9, 2) + "B"
	case f >= 1e6:
		return trimNum(f/1e6, 1) + "M"
	case f >= 1e3:
		return trimNum(f/1e3, 1) + "K"
	}
	return fmtInt(n)
}

func trimNum(f float64, prec int) string {
	if f >= 100 {
		prec = 0
	} else if f >= 10 && prec > 1 {
		prec = 1
	}
	s := fmt.Sprintf("%.*f", prec, f)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

// fmtInt groups thousands.
func fmtInt(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtCost(usd float64) string {
	switch {
	case usd < 0:
		// an overdrawn relay wallet
		return "−" + fmtCost(-usd)
	case usd == 0:
		return "$0"
	case usd < 0.01:
		return "<$0.01"
	case usd < 100:
		return fmt.Sprintf("$%.2f", usd)
	case usd < 10000:
		return "$" + fmtInt(int64(math.Round(usd)))
	}
	return "$" + trimNum(usd/1000, 1) + "K"
}

// fmtCostOf formats a cost that may lack a price: "—" when nothing in it
// was priced, the priced part with a trailing "+" when only some was.
func fmtCostOf(usd float64, requests, unpriced int64) string {
	switch {
	case requests > 0 && unpriced >= requests:
		return "—"
	case unpriced > 0:
		return fmtCost(usd) + "+"
	}
	return fmtCost(usd)
}

func fmtPct(f float64) string {
	if f <= 0 {
		return "0%"
	}
	if f < 0.001 {
		return "<0.1%"
	}
	return trimNum(f*100, 1) + "%"
}

// fmtAgo is a relative time.
func fmtAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return tr("lessThanMin")
	case d < time.Hour:
		return trf("minAgo", int(d.Minutes()))
	case d < 24*time.Hour:
		return trf("hourAgo", int(d.Hours()))
	case d < 30*24*time.Hour:
		return trf("dayAgo", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

func fmtWhen(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

// attribLabel is the short label of an attribution source, "" for log.
func attribLabel(a model.AttribSource) string {
	switch a {
	case model.AttribInferred:
		return tr("inferred")
	case model.AttribCCSwitch:
		return tr("viaCCSwitch")
	case model.AttribConfig:
		return tr("viaConfig")
	case model.AttribUserRule:
		return tr("viaRule")
	}
	return ""
}

// shortPath shows a project path with ~ and at most the last two parts.
func shortPath(p string) string {
	if p == "" {
		return "—"
	}
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		p = "~" + p[len(h):]
	}
	return p
}

// baseName is the last element of a path.
func baseName(p string) string {
	p = strings.TrimRight(p, "/\\")
	if i := strings.LastIndexAny(p, "/\\"); i >= 0 {
		return p[i+1:]
	}
	if p == "" {
		return "—"
	}
	return p
}
