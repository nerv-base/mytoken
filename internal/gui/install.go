package gui

import (
	"context"

	"github.com/egoist/mygo/ui"

	"github.com/zzstar101/mytoken/internal/model"
)

// installHint tells how to get a tool MyToken reads: a command to paste
// into a terminal, where there is one, and its home page.
type installHint struct {
	cmd string
	url string
}

// installHints lists the tools by their usual install. Editors and apps
// have no command, only a page to download from.
var installHints = map[model.Harness]installHint{
	model.ClaudeCode:    {"curl -fsSL https://claude.ai/install.sh | bash", "https://claude.com/product/claude-code"},
	model.Codex:         {"curl -fsSL https://chatgpt.com/codex/install.sh | sh", "https://github.com/openai/codex"},
	model.Gemini:        {"npm install -g @google/gemini-cli", "https://github.com/google-gemini/gemini-cli"},
	model.DSH:           {"npx @deepseek-ai/dsh web", "https://www.deepseek.com/harness/"},
	model.OpenCode:      {"curl -fsSL https://opencode.ai/install | bash", "https://opencode.ai"},
	model.Crush:         {"brew install charmbracelet/tap/crush", "https://github.com/charmbracelet/crush"},
	model.Cline:         {"npm install -g cline", "https://cline.bot"},
	model.Roo:           {"", "https://github.com/RooCodeInc/Roo-Code"},
	model.Kilo:          {"", "https://kilo.ai"},
	model.Pi:            {"curl -fsSL https://pi.dev/install.sh | sh", "https://pi.dev"},
	model.Grok:          {"curl -fsSL https://x.ai/cli/install.sh | bash", "https://github.com/xai-org/grok-build"},
	model.OpenClaw:      {"curl -fsSL https://openclaw.ai/install.sh | bash", "https://openclaw.ai"},
	model.Droid:         {"curl -fsSL https://app.factory.ai/cli | sh", "https://factory.ai"},
	model.Goose:         {"curl -fsSL https://github.com/aaif-goose/goose/releases/download/stable/download_cli.sh | bash", "https://goose-docs.ai"},
	model.Forge:         {"curl -fsSL https://forgecode.dev/cli | sh", "https://forgecode.dev"},
	model.ClaudeDesktop: {"", "https://claude.ai/download"},
	model.Qwen:          {"curl -fsSL https://qwen-code-assets.oss-cn-hangzhou.aliyuncs.com/installation/install-qwen-standalone.sh | bash", "https://github.com/QwenLM/qwen-code"},
	model.Kimi:          {"curl -fsSL https://code.kimi.com/kimi-code/install.sh | bash", "https://github.com/MoonshotAI/kimi-code"},
	model.OpenClaude:    {"npm install -g @twigpine/openclaude@latest", "https://github.com/Twigpine/openclaude"},
	model.WorkBuddy:     {"", "https://www.codebuddy.cn/work"},
	model.CodeBuddy:     {"npm install -g @tencent-ai/codebuddy-code", "https://www.codebuddy.ai"},
	model.Hermes:        {"curl -fsSL https://hermes-agent.nousresearch.com/install.sh | bash", "https://hermes-agent.nousresearch.com/"},
	model.Amp:           {"curl -fsSL https://ampcode.com/install.sh | bash", "https://ampcode.com"},
	model.Kiro:          {"curl -fsSL https://cli.kiro.dev/install | bash", "https://kiro.dev"},
	model.Codebuff:      {"npm install -g codebuff", "https://www.codebuff.com"},
	model.Mux:           {"npm install -g @coder/xum", "https://xum.cdr.dev"},
	model.Zed:           {"", "https://zed.dev"},
	model.Antigravity:   {"", "https://antigravity.google"},
	model.ZCode:         {"", "https://zcode.z.ai"},
	model.MiniMaxCode:   {"curl -fsSL https://filecdn.minimax.chat/public/install.sh | bash", "https://github.com/MiniMax-AI/minimax-code"},
	model.MiMoCode:      {"curl -fsSL https://mimo.xiaomi.com/install | bash", "https://github.com/XiaomiMiMo/MiMo-Code"},
	model.StepCode:      {"curl -fsSL https://static-openapi.stepfun.com/stepcode/install.sh | bash", "https://www.stepfun.com/stepcode"},
	model.Trae:          {"", "https://www.trae.ai/"},
}

// installCard lists the tools not found on this machine, each with how to
// install it, and a way to point at its folder if it is installed after
// all where detection does not look.
func (a *App) installCard(c *ui.Context, pal palette, missing []Source) {
	st := &a.set
	card(c, pal).Gap(6).Children(func() {
		cardTitle(c, pal, trf("installOthers", len(missing)), nil)
		caption(c, pal, tr("installOthersHint"))
		for i, s := range missing {
			if i > 0 {
				ui.Divider(c)
			}
			h := s.Harness
			hint := installHints[h]
			ui.Row(c.Key(string(h))).Gap(12).AlignItems(ui.Center).Padding(6, 0).Children(func() {
				toolBadge(c, h, 26).Opacity(0.85)
				ui.Column(c).Gap(5).Grow(1).Basis(0).MinWidth(0).Children(func() {
					ui.Text(c, h.DisplayName()).FontSize(13.5).FontWeight(600).TextColor(pal.ink)
					if hint.cmd != "" {
						a.copyCmd(c, pal, hint.cmd)
					}
				})
				if hint.url != "" {
					if ui.Button(c, tr("website")).Clicked() {
						c.OpenURL(hint.url)
					}
				}
				if a.d.PickFolder != nil {
					if ui.Button(c, tr("installedDir")).Disabled(st.act.running).Clicked() {
						if dir, ok := a.d.PickFolder(""); ok {
							a.act(trf("dirSet", h.DisplayName()), func(ctx context.Context) error {
								if err := a.d.Sources.SetRoot(ctx, h, dir); err != nil {
									return err
								}
								return a.d.Sources.SetEnabled(ctx, h, true)
							})
						}
					}
				}
			})
		}
	})
}

// copyCmd is a command in a monospaced well; clicking copies it.
func (a *App) copyCmd(c *ui.Context, pal palette, cmd string) {
	b := ui.Row(c.Key("cmd")).Gap(8).AlignItems(ui.Center).Padding(4, 8).Radius(7).Cursor(ui.CursorPointer).MinWidth(0).AlignSelf(ui.Start)
	bg := pal.well
	if b.Hovered() {
		bg = pal.well.Mix(pal.ink, 0.06)
	}
	b.Background(b.AnimateColor("bg", bg, quick))
	press(b)
	b.Children(func() {
		ui.Text(c, cmd).Font("SF Mono, Menlo, Consolas, monospace").FontSize(11.5).TextColor(pal.ink).SingleLine().Ellipsis("…").Shrink(1).MinWidth(0)
		ui.Text(c, tr("copy")).FontSize(11).TextColor(pal.muted).NoWrap()
	})
	if b.Clicked() {
		c.WriteClipboard(cmd)
		a.set.notice = tr("copied")
		a.set.err = ""
	}
}
