package gui

import (
	"fmt"
	"os"
	"strings"
)

// lang is the interface language: "zh" or "en".
var lang = "en"

// setLang sets the language; "" follows the system (MYTOKEN_LANG, then
// LANG and the like).
func setLang(l string) {
	switch l {
	case "zh", "en":
		lang = l
		return
	}
	lang = systemLang()
}

// SetLang sets the interface language ("zh", "en", "" = system).
func SetLang(l string) { setLang(l) }

// Lang is the interface language in use: "zh" or "en".
func Lang() string { return lang }

func systemLang() string {
	for _, k := range []string{"MYTOKEN_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.ToLower(os.Getenv(k)); v != "" {
			if strings.HasPrefix(v, "zh") {
				return "zh"
			}
			return "en"
		}
	}
	return systemLocale()
}

// SetSystemLang lets main pass the locale the OS reports, for when the
// environment does not carry one (apps launched from Finder).
func SetSystemLang(locale string) {
	sysLocale = strings.ToLower(locale)
}

var sysLocale string

func systemLocale() string {
	if strings.HasPrefix(sysLocale, "zh") {
		return "zh"
	}
	return "en"
}

func tr(key string) string {
	if m, ok := strs[key]; ok {
		if lang == "zh" {
			return m[1]
		}
		return m[0]
	}
	return key
}

func trf(key string, args ...any) string { return fmt.Sprintf(tr(key), args...) }

// strs holds each string in English and Chinese.
var strs = map[string][2]string{
	// Navigation
	"navigation": {"Navigation", "导航"},
	"overview":   {"Overview", "概览"},
	"sessions":   {"Sessions", "会话"},
	"ranking":    {"Ranking", "排行"},
	"projects":   {"Projects", "项目"},
	"settings":   {"Settings", "设置"},

	// Times
	"justNow":    {"just now", "刚刚"},
	"minAgo":     {"%dm ago", "%d 分钟前"},
	"hourAgo":    {"%dh ago", "%d 小时前"},
	"dayAgo":     {"%dd ago", "%d 天前"},
	"resetSoon":  {"resets soon", "即将重置"},
	"resetsInM":  {"resets in %dm", "%d 分钟后重置"},
	"resetsInHM": {"resets in %dh %dm", "%d 小时 %d 分后重置"},
	"resetsInD":  {"resets in %dd", "%d 天后重置"},
	"daysN":      {"%d days", "%d 天"},
	"rateFmt":    {"~%s tok/s", "约 %s tok/s"},

	// Ranges
	"range":    {"Range", "范围"},
	"range7":   {"7 days", "7 天"},
	"range30":  {"30 days", "30 天"},
	"range90":  {"90 days", "90 天"},
	"rangeAll": {"All", "全部"},

	// Common
	"today":         {"Today", "今日"},
	"apiEquivalent": {"API equivalent", "API 等价"},
	"costNote":      {"Costs are official API list prices, not what you paid.", "花费按官方 API 价格折算，不代表实际支付金额。"},
	"loading":       {"Loading…", "加载中…"},
	"noData":        {"No usage yet", "还没有用量"},
	"noDataHint":    {"MyToken reads the logs your AI coding tools keep. Use one, or check Settings → Sources.", "MyToken 读取 AI 编程工具留下的日志。用一次这些工具，或到 设置 → 数据来源 看看。"},
	"noUsage":       {"No usage in this range.", "这段时间没有用量。"},
	"unpriced":      {"Unpriced", "未定价"},
	"unknown":       {"Unknown", "未知"},
	"other":         {"Other", "其他"},
	"tokens":        {"Tokens", "Token"},
	"tokensN":       {"%s tokens", "%s Token"},
	"requests":      {"Requests", "请求"},
	"requestsN":     {"%d requests", "%d 次请求"},
	"sessionsN":     {"Sessions", "会话"},
	"hitRate":       {"Cache hit", "缓存命中"},
	"hitRateLong":   {"of input read from cache", "的输入来自缓存"},
	"input":         {"Input", "输入"},
	"output":        {"Output", "输出"},
	"cacheRead":     {"Cache read", "缓存读取"},
	"cacheWrite":    {"Cache write", "缓存写入"},
	"reasoning":     {"Reasoning", "推理"},
	"cost":          {"Cost", "花费"},
	"model":         {"Model", "模型"},
	"time":          {"Time", "时间"},
	"duration":      {"Duration", "时长"},
	"activeN":       {"%d running", "%d 个进行中"},
	"unavailable":   {"Not available", "不可用"},

	// Overview
	"todaySpend":  {"Today", "今日花费"},
	"vsYesterday": {"vs yesterday", "较昨日"},
	"vsLastMonth": {"vs last month", "较上月"},
	"unpricedN":   {"%d requests without a known price are not counted.", "有 %d 次请求的模型没有已知价格，未计入。"},
	"forecast":    {"Month forecast", "本月预测"},
	"monthDay":    {"Day %d of %d", "第 %d 天 / 共 %d 天"},
	"planQuota":   {"Plan quota", "套餐额度"},
	"viaAPI":      {"from the service", "来自服务端"},
	"viaLog":      {"from local logs", "来自本地日志"},
	"win5h":       {"5-hour", "5 小时"},
	"winWeek":     {"Weekly", "每周"},
	"winMonth":    {"Monthly", "每月"},
	"winDay":      {"Daily", "每日"},
	"dailySpend":  {"Daily spend", "每日花费"},
	"byTool":      {"By tool", "按工具"},
	"byModel":     {"By model", "按模型"},

	// Sessions
	"sortRecent":      {"Newest", "最新"},
	"sortCost":        {"Costliest", "最贵"},
	"allTools":        {"All tools", "全部工具"},
	"searchSessions":  {"Search title, project, tool, model", "搜索标题、项目、工具、模型"},
	"noSessions":      {"No sessions", "没有会话"},
	"sessionsCount":   {"%d sessions", "%d 个会话"},
	"sessionsMatch":   {"%d of %d sessions", "%d / %d 个会话"},
	"sessionsNewest":  {"Newest %d of %d sessions", "最近 %d 个，共 %d 个会话"},
	"pickSession":     {"Choose a session", "选择一个会话"},
	"pickSessionHint": {"Its requests, models and cost over time show here.", "这里会显示它的请求、模型和花费变化。"},
	"subagentsN":      {"%d subagents", "%d 个子代理"},
	"costOverTime":    {"Cost over time", "花费累计"},

	// Ranking
	"costRanking":  {"By cost", "花费排行"},
	"rankBy":       {"Rank by", "排行维度"},
	"dimTool":      {"Tool", "工具"},
	"dimModel":     {"Model", "模型"},
	"dimProvider":  {"Provider", "提供商"},
	"rankSub":      {"%s tokens · %s requests", "%s Token · %s 次请求"},
	"yearActivity": {"The past year", "过去一年"},
	"streakNow":    {"Current streak", "当前连续"},
	"streakBest":   {"Longest streak", "最长连续"},
	"activeDays":   {"Days used", "使用天数"},
	"topDay":       {"Biggest day", "花费最多的一天"},
	"less":         {"Less", "少"},
	"more":         {"More", "多"},

	// Projects
	"projectsCount": {"%d projects", "%d 个项目"},
	"projectSub":    {"%s sessions · %s tokens", "%s 个会话 · %s Token"},
	"noProject":     {"No project", "无项目"},
	"pickProject":   {"Choose a project", "选择一个项目"},
	"topSessions":   {"Most expensive sessions", "花费最多的会话"},

	// Tray
	"activeNow":     {"Running now", "正在进行"},
	"rateEstimated": {"output speed, estimated", "输出速度为估算"},
	"noActive":      {"Nothing running in the last 15 minutes.", "最近 15 分钟没有进行中的会话。"},
	"monthSoFar":    {"This month %s", "本月 %s"},
	"perHour":       {"%s/h", "%s/小时"},
	"openMain":      {"Open MyToken", "打开 MyToken"},
	"quit":          {"Quit", "退出"},

	// Settings
	"set_general":        {"General", "通用"},
	"set_sources":        {"Sources", "数据来源"},
	"set_prices":         {"Prices", "价格"},
	"set_data":           {"Data", "数据维护"},
	"language":           {"Language", "语言"},
	"lang_":              {"System", "跟随系统"},
	"lang_zh":            {"中文", "中文"},
	"lang_en":            {"English", "English"},
	"appearance":         {"Appearance", "外观"},
	"look_":              {"System", "跟随系统"},
	"look_light":         {"Light", "浅色"},
	"look_dark":          {"Dark", "深色"},
	"trayTitle":          {"Menu bar text", "菜单栏文字"},
	"trayTitleHint":      {"What shows beside the menu bar icon.", "菜单栏图标旁显示的内容。"},
	"tray_cost":          {"Today's spend", "今日花费"},
	"tray_rate":          {"Output speed", "输出速度"},
	"tray_hit":           {"Cache hit rate", "缓存命中率"},
	"tray_quota":         {"Quota used", "额度用量"},
	"tray_none":          {"Icon only", "仅图标"},
	"about":              {"About", "关于"},
	"aboutHint":          {"Reads your AI coding tools' local logs. Nothing leaves this machine except quota checks with the tools' own credentials.", "读取 AI 编程工具的本地日志。除用工具自带凭据查询额度外，数据不会离开本机。"},
	"pricesFrom":         {"Official prices", "官方价格"},
	"pricesFromHint":     {"Costs use each model's official API list price, with cache reads and writes at their own rates.", "花费按各模型的官方 API 价格计算，缓存读写按各自价格计。"},
	"builtinCatalog":     {"built in", "内置"},
	"catalogInfo":        {"Price list %s · %d models", "价格表 %s · %d 个模型"},
	"sourcesFound":       {"Found %d tools", "已检测到 %d 个工具"},
	"sourcesHint":        {"MyToken finds tools where they keep their data. Turn one off to stop reading it; choose a folder only if a tool keeps its data somewhere else.", "MyToken 会在工具默认的位置查找数据。关闭开关即不再读取；只有工具把数据放在别处时才需要手动选择目录。"},
	"sourcesMissing":     {"%d tools not found", "%d 个工具未检测到"},
	"sourcesMissingHint": {"Installed one of these but it is missing? Choose the folder it keeps its data in.", "安装了却没检测到？选择它保存数据的目录。"},
	"show":               {"Show", "显示"},
	"timezone":           {"Time zone", "时区"},
	"timezoneHint":       {"Days and months are counted in it. System: %s", "按这个时区划分每天和每月。系统：%s"},
	"followSystem":       {"Follow system", "跟随系统"},
	"currency":           {"Currency", "货币"},
	"currencyHint":       {"Prices are published in US dollars; other currencies use your rate.", "价格按美元公布；其他货币按你填写的汇率换算。"},
	"openAtLogin":        {"Open at login", "开机启动"},
	"openAtLoginHint":    {"Start MyToken in the menu bar when you log in.", "登录时在菜单栏启动 MyToken。"},
	"saved":              {"Saved.", "已保存。"},
	"rescanHint":         {"Read every source again for new requests.", "重新读取所有数据来源里的新请求。"},
	"export":             {"Export", "导出"},
	"exportHint":         {"Every request with its tokens and API-equivalent cost, as CSV.", "导出每条请求的 token 与 API 等价花费（CSV）。"},
	"exportCSV":          {"Export CSV…", "导出 CSV…"},
	"exported":           {"Exported to %s.", "已导出到 %s。"},
	"rescan":             {"Rescan", "重新扫描"},
	"rescanDone":         {"Rescanned.", "已重新扫描。"},
	"srcDetected":        {"Detected", "已检测到"},
	"srcCustom":          {"Custom folder", "自定义目录"},
	"srcProblem":         {"Problem", "有问题"},
	"srcStats":           {"%s requests · last %s", "%s 次请求 · 最近 %s"},
	"chooseDir":          {"Choose folder…", "选择目录…"},
	"autoDir":            {"Use default", "恢复默认"},
	"dirSet":             {"%s now reads the chosen folder.", "%s 已改为读取所选目录。"},
	"dirAuto":            {"%s reads its default folder again.", "%s 已恢复读取默认目录。"},
	"unpricedModels":     {"Models without a price (%d)", "没有价格的模型（%d）"},
	"allPriced":          {"Every model in use has a price.", "所有在用的模型都有价格。"},
	"unpricedHint":       {"Price a model as an official one it is the same as, or give it a price.", "把它当作某个官方模型计价，或者直接给它定价。"},
	"priceAs":            {"Price as %s", "按 %s 计价"},
	"customPrice":        {"Set a price…", "自定义价格…"},
	"aliases":            {"Model aliases", "模型别名"},
	"aliasesHint":        {"A model name your tool reports, priced as an official model.", "工具上报的模型名，按某个官方模型计价。"},
	"aliasFrom":          {"Reported name", "上报的模型名"},
	"aliasTo":            {"Official model", "官方模型"},
	"add":                {"Add", "添加"},
	"aliasAdded":         {"%s is now priced as %s.", "%s 已按 %s 计价。"},
	"customPrices":       {"Custom prices", "自定义价格"},
	"customPricesHint":   {"USD per million tokens, for models with no official price.", "单位为美元 / 百万 Token，用于没有官方价格的模型。"},
	"priceLine":          {"in %s · out %s · read %s · write %s", "输入 %s · 输出 %s · 读 %s · 写 %s"},
	"savePrice":          {"Save price", "保存价格"},
	"priceSaved":         {"Saved the price of %s.", "已保存 %s 的价格。"},
	"delete":             {"Delete", "删除"},
	"deleteAsk":          {"Delete %s?", "删除 %s？"},
	"deleteAskHint":      {"Its requests are priced again without it.", "删除后相关请求会重新计价。"},
	"deleted":            {"Deleted %s.", "已删除 %s。"},
	"cancel":             {"Cancel", "取消"},
	"database":           {"Local index", "本地索引"},
	"reveal":             {"Show file", "显示文件位置"},
	"dbSize":             {"Size", "大小"},
	"dbRange":            {"From %s to %s", "%s 至 %s"},
	"maintenance":        {"Maintenance", "维护"},
	"compact":            {"Compact", "压缩"},
	"compactHint":        {"Give unused space in the index back to the disk.", "把索引里未使用的空间还给磁盘。"},
	"compactDone":        {"Compacted.", "已压缩。"},
	"rebuild":            {"Rebuild", "重建"},
	"rebuildHint":        {"Empty the index and read every tool's logs again.", "清空索引，重新读取所有工具的日志。"},
	"rebuildAsk":         {"Rebuild the index?", "重建索引？"},
	"rebuildAskHint":     {"Usage whose logs the tools have since deleted is lost.", "工具已经删除日志的那部分用量将会丢失。"},
	"rebuildDone":        {"Rebuilt.", "已重建。"},
	"working":            {"Working…", "处理中…"},
}
