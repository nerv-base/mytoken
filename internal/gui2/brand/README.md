# Harness logos

These SVGs are the harnesses' own marks, taken unmodified from
[@lobehub/icons-static-svg](https://www.npmjs.com/package/@lobehub/icons-static-svg)
1.95.1 (MIT). The logos remain trademarks of their owners and are used only
to identify each tool.

| File | Harness |
|------|---------|
| claudecode-color.svg | Claude Code |
| openai.svg | Codex |
| gemini-color.svg | Gemini CLI |
| deepseek-color.svg | DeepSeek Harness |
| opencode.svg | OpenCode |
| cline.svg | Cline |
| roocode.svg | Roo Code |
| kilocode.svg | Kilo Code |
| pi.svg | Pi |
| claude-color.svg | Claude Desktop |
| codebuddy-color.svg | CodeBuddy |
| hermesagent.svg | Hermes Agent |
| qwen-color.svg | Qwen Code |
| kimi-color.svg | Kimi CLI |
| grok.svg | Grok CLI |
| openclaw-color.svg | OpenClaw |
| goose.svg | Goose |
| amp-color.svg | Amp |
| kiro-color.svg | Kiro |
| antigravity-color.svg | Antigravity |
| antigravity.svg | Antigravity (one-color, unused for now) |

## Not from @lobehub/icons

These are the tools' own published brand assets, fetched 2026-10-10.
They stay the trademarks of their owners and identify the tool only.

| File | Harness | Source | Terms | Change made here |
|------|---------|--------|-------|------------------|
| workbuddy-color.svg | WorkBuddy | its app bundle, `renderer/assets/logo-workbuddy-*.svg` | bundled app asset | the blurred glow became a radial gradient, because MyGo draws no SVG filters |
| droid-color.svg | Droid | <https://factory.ai/favicon.svg>, identical at <https://docs.factory.ai/favicon.svg> | © Factory AI, no license granted with the file; used nominatively | the favicon's outer `<svg>` wrapper, its trailing `<style>` media queries and the `<g clip-path>` wrapper (whose `clipPath` lived in the removed wrapper) were dropped; geometry unchanged |
| forge.svg | Forge | <https://forgecode.dev/shared/brand/favicon-dark.svg> | © Antinomy, used nominatively | fill `white` became `currentColor`, so the app tints it |
| zed.svg | Zed | `assets/images/zed_logo.svg` in [zed-industries/zed](https://github.com/zed-industries/zed), fetched 2026-10-11 | © Zed Industries, used nominatively | the full-canvas `clipPath` wrapper was dropped and fill `black` became `currentColor` |
| mux.svg | Mux | <https://mux.coder.com/favicon.svg>, fetched 2026-10-11 | © Coder, used nominatively | its `<style>` light/dark color block was dropped; the paths already use `currentColor` |
| openclaude-color.svg | OpenClaude | `docs/assets/openclaude-wordmark.svg` in [Gitlawb/openclaude](https://github.com/Gitlawb/openclaude) | MIT | the wordmark's first CLAUDE letter, translated to the origin; the project publishes no square icon. Its badge tint in `internal/gui/theme.go` is the same green (`#4EBA65`) |

Forge's mark is ForgeCode's (antinomyhq/forge, installed from forgecode.dev,
which keeps its data under `~/.forge`, and under `~/forge` in older releases).

Codebuff publishes no vector mark (only bitmap favicons), so the app shows a
dot in its color instead.
