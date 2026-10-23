# Provider logos

Provider logo SVGs used in the project README to indicate MCP-client compatibility.

## Source and licence

These icons are sourced from [LobeHub / lobe-icons](https://github.com/lobehub/lobe-icons) and are distributed under the MIT licence. The complete original copyright and permission notice is preserved in [LICENSE](LICENSE), copied verbatim from the [upstream licence](https://github.com/lobehub/lobe-icons/blob/master/LICENSE).

```
MIT License — Copyright (c) 2023 LobeHub
https://github.com/lobehub/lobe-icons/blob/master/LICENSE
```

The brand names, logos and trademarks displayed remain the property of their respective owners (Anthropic, OpenAI, Mistral AI, Google). They are used here for nominative identification of MCP-compatible client applications and do not imply endorsement.

## Files

| File | Used for | Source variant |
|------|----------|----------------|
| `claude.svg` | Claude (claude.ai) | `claude-color` (coloured) |
| `openai.svg` | ChatGPT | `openai` (monochrome), recoloured to ChatGPT teal `#10A37F` |
| `mistral.svg` | Le Chat (Mistral) | `mistral-color` (coloured) |
| `gemini.svg` | Gemini | `gemini-color` (coloured) |

All four logos carry an explicit `fill` so they remain visible on both light and dark themes. Lobe-icons' `currentColor` variants would only render correctly when embedded inline with surrounding CSS — GitHub serves SVGs through `<img>`, which strips that context.
