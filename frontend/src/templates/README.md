# Chat template references

Vendored `chat_template` strings extracted from the official Qwen
`tokenizer_config.json` files. They are reference data, never executed: the
frontend parses them to derive the template's closed-scope grammar
(segment markers plus XML-style tag pairs).

| File | Source | SHA-256 (file incl. trailing newline) |
|---|---|---|
| `qwen2.5-7b-instruct.chat_template.jinja` | https://huggingface.co/Qwen/Qwen2.5-7B-Instruct/raw/main/tokenizer_config.json | `cd8e9439f0570856fd70470bf8889ebd8b5d1107207f67a5efb46e342330527f` |
| `qwen3-8b.chat_template.jinja` | https://huggingface.co/Qwen/Qwen3-8B/raw/main/tokenizer_config.json | `e132ae041e1217b5e1114eb9dc292484a7f478df945d72fa49ba01b88d8ec01a` |

Extraction command:

```bash
python3 - <<'PY'
import json, urllib.request, pathlib
url = "https://huggingface.co/Qwen/<repo>/raw/main/tokenizer_config.json"
data = json.loads(urllib.request.urlopen(url, timeout=30).read().decode())
template = data["chat_template"]
if isinstance(template, list):
    template = "\n\n".join(t.get("template", "") for t in template)
pathlib.Path("out.jinja").write_text(template if template.endswith("\n") else template + "\n")
PY
```

Licenses: Qwen2.5 and Qwen3 weights/configs are Apache-2.0.

If a template changes upstream, re-extract and update the hash here; the
scope registry is derived at runtime, so the UI follows the data.
