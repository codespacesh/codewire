---
display_name: Codewire
description: Persistent process server for AI coding agents
icon: ../.icons/codewire.svg
maintainer_github: codespacesh
tags: [agent, codewire, terminal]
---

# Codewire

Install and run [Codewire](https://github.com/codespacesh/codewire) in your Coder workspace. Codewire is a persistent process server for AI coding agents — it manages terminal sessions that survive reconnects.

```tf
module "codewire" {
  source   = "github.com/codespacesh/codewire//coder-module"
  agent_id = coder_agent.main.id
  folder   = "/home/coder/project"
}
```

## What this module does

1. Installs the `cw` binary via the official install script
2. Starts the codewire daemon (`cw daemon`) on workspace start
3. Adds a **Codewire** app button to the Coder dashboard that launches a new terminal session

## Variables

| Variable | Type | Default | Description |
|----------|------|---------|-------------|
| `agent_id` | string | required | The ID of a Coder agent |
| `order` | number | `null` | App position in the UI |
| `icon` | string | `/icon/terminal.svg` | App icon |
| `folder` | string | `/home/coder` | Working directory for sessions |
| `install_codewire` | bool | `true` | Whether to install cw |
| `codewire_version` | string | `"latest"` | Version to install (e.g. `v0.1.0`) |
| `experiment_report_tasks` | bool | `false` | Enable Coder MCP task reporting |

## Examples

### Pin a specific version

```tf
module "codewire" {
  source           = "github.com/codespacesh/codewire//coder-module"
  agent_id         = coder_agent.main.id
  codewire_version = "v0.1.0"
}
```

### Skip installation (pre-installed in image)

```tf
module "codewire" {
  source           = "github.com/codespacesh/codewire//coder-module"
  agent_id         = coder_agent.main.id
  install_codewire = false
}
```

### Enable task reporting (experimental)

```tf
module "codewire" {
  source                  = "github.com/codespacesh/codewire//coder-module"
  agent_id                = coder_agent.main.id
  experiment_report_tasks = true
}
```
