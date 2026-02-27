terraform {
  required_version = ">= 1.0"

  required_providers {
    coder = {
      source  = "coder/coder"
      version = ">= 0.17"
    }
  }
}

variable "agent_id" {
  type        = string
  description = "The ID of a Coder agent."
}

variable "order" {
  type        = number
  description = "The order determines the position of app in the UI presentation. The lowest order is shown first and apps with equal order are sorted by name (ascending order)."
  default     = null
}

variable "icon" {
  type        = string
  description = "The icon to use for the app."
  default     = "/icon/terminal.svg"
}

variable "folder" {
  type        = string
  description = "The working directory for codewire sessions."
  default     = "/home/coder"
}

variable "install_codewire" {
  type        = bool
  description = "Whether to install codewire."
  default     = true
}

variable "codewire_version" {
  type        = string
  description = "The version of codewire to install (e.g. v0.1.0)."
  default     = "latest"
}

variable "relay_url" {
  type        = string
  default     = ""
  description = "Codewire relay URL (e.g. https://user.relay.codewire.sh). Leave empty for standalone mode."
}

variable "relay_token" {
  type        = string
  default     = ""
  sensitive   = true
  description = "Invite/admin token for token-mode relays. Leave empty to use OIDC device flow."
}

variable "experiment_report_tasks" {
  type        = bool
  description = "Whether to enable Coder MCP task reporting."
  default     = false
}

resource "coder_script" "codewire" {
  agent_id     = var.agent_id
  display_name = "Codewire"
  icon         = var.icon
  script       = <<-EOT
    #!/bin/bash
    set -e

    # Install codewire if enabled
    if [ "${var.install_codewire}" = "true" ]; then
      echo "Installing codewire..."
      INSTALL_ARGS=""
      if [ "${var.codewire_version}" != "latest" ]; then
        INSTALL_ARGS="--version ${var.codewire_version}"
      fi
      curl -fsSL https://raw.githubusercontent.com/codewiresh/codewire/main/install.sh | bash -s -- $INSTALL_ARGS
    fi

    # Verify cw is installed
    if ! command -v cw >/dev/null 2>&1; then
      echo "Error: cw is not installed. Enable install_codewire or install it manually."
      exit 1
    fi

    # Configure Coder MCP task reporting if enabled
    if [ "${var.experiment_report_tasks}" = "true" ]; then
      echo "Configuring Coder MCP task reporting..."
      coder exp mcp configure claude-code ${var.folder}
    fi

    # Relay registration (only if relay_url is set and not already registered)
    %{~ if var.relay_url != "" ~}
    if ! grep -q "relay_token" "$HOME/.codewire/config.toml" 2>/dev/null; then
      echo "Registering with Codewire relay..."
      %{~ if var.relay_token != "" ~}
      cw setup "${var.relay_url}" "${var.relay_token}"
      %{~ else ~}
      cw setup "${var.relay_url}"
      %{~ endif ~}
    fi
    %{~ endif ~}

    # Start the codewire node in the background
    echo "Starting codewire node..."
    cw node &
  EOT
  run_on_start = true
}

resource "coder_app" "codewire" {
  slug         = "codewire"
  display_name = "Codewire"
  agent_id     = var.agent_id
  icon         = var.icon
  order        = var.order
  command      = "cw launch --dir ${var.folder} -- bash"
}
