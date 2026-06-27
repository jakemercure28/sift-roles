#!/bin/bash
#
# Sift installer for Mac.
#
# Run this one line in Terminal:
#
#   /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/jakemercure28/job-search-automation/main/install.sh)"
#
# It installs everything it needs, downloads the app, and opens the dashboard
# in your browser by itself. You will be asked for your password once, and a
# couple of system windows will pop up that you just click through.
#
# Safe to run again if it stops partway. It skips anything already done.

set -euo pipefail

REPO_URL="https://github.com/jakemercure28/job-search-automation.git"
INSTALL_DIR="$HOME/job-search-automation"
TOTAL_STEPS=6

# ----- pretty output helpers (no fancy dashes, per repo writing rule) -----
say()   { printf "\n%s\n" "$*"; }
step()  { printf "\n========================================\n  Step %s of %s: %s\n========================================\n" "$1" "$TOTAL_STEPS" "$2"; }
ok()    { printf "  OK. %s\n" "$*"; }
wait_note() { printf "  This can take a few minutes. Leave this window open.\n"; }

# Only prompt for a keypress when a person is actually at the keyboard.
pause() { if [ -t 0 ]; then read -r -p "$1" _ || true; fi; }

trap 'printf "\nSomething stopped early. You can safely run the same command again to continue.\nIt picks up where it left off.\n"; pause "Press Return to close."' ERR

# Same script handles first install and updates. Detect which so the messaging
# matches what is actually happening.
if [ -d "$INSTALL_DIR/.git" ] || git rev-parse --show-toplevel >/dev/null 2>&1; then
  MODE="update"
  say "Sift update is starting."
else
  MODE="install"
  say "Sift setup is starting."
fi
say "Sit tight. The dashboard opens in your browser automatically when it is done."

# ----------------------------------------------------------------------------
step 1 "Apple developer tools"
# ----------------------------------------------------------------------------
if xcode-select -p >/dev/null 2>&1; then
  ok "Developer tools already installed."
else
  say "  A system window will pop up. Click Install and wait for it to finish."
  wait_note
  xcode-select --install >/dev/null 2>&1 || true
  # The install runs in a separate GUI process. Block until it is present.
  until xcode-select -p >/dev/null 2>&1; do
    sleep 10
  done
  ok "Developer tools installed."
fi

# ----------------------------------------------------------------------------
step 2 "Homebrew (the installer Apps use)"
# ----------------------------------------------------------------------------
# Figure out where brew lives on this Mac (Apple Silicon vs Intel).
if [ -x /opt/homebrew/bin/brew ]; then
  BREW_BIN=/opt/homebrew/bin/brew
elif [ -x /usr/local/bin/brew ]; then
  BREW_BIN=/usr/local/bin/brew
else
  BREW_BIN=""
fi

if [ -n "$BREW_BIN" ] && command -v brew >/dev/null 2>&1; then
  ok "Homebrew already installed."
else
  if [ -z "$BREW_BIN" ]; then
    say "  Installing Homebrew. You will be asked for your Mac password once."
    wait_note
    NONINTERACTIVE=1 /bin/bash -c \
      "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
    if [ -x /opt/homebrew/bin/brew ]; then
      BREW_BIN=/opt/homebrew/bin/brew
    else
      BREW_BIN=/usr/local/bin/brew
    fi
  fi
  # Persist brew on PATH for future zsh terminals, and load it for this run.
  SHELLENV_LINE="eval \"\$($BREW_BIN shellenv)\""
  if ! grep -qF "$SHELLENV_LINE" "$HOME/.zprofile" 2>/dev/null; then
    printf '\n%s\n' "$SHELLENV_LINE" >> "$HOME/.zprofile"
  fi
  eval "$("$BREW_BIN" shellenv)"
  ok "Homebrew installed."
fi

# Ensure brew is usable for the rest of this run regardless of branch above.
eval "$("$BREW_BIN" shellenv)"

# ----------------------------------------------------------------------------
step 3 "Container engine (runs the app)"
# ----------------------------------------------------------------------------
engine_up()  { docker info >/dev/null 2>&1; }
compose_up() { docker compose version >/dev/null 2>&1; }

# Wait up to 5 minutes for an engine to answer, then stop with a retry hint
# instead of hanging forever.
wait_for_engine() {
  local label="$1" n=0
  say "  Waiting for $label to be ready."
  until engine_up; do
    n=$((n + 1))
    if [ "$n" -gt 60 ]; then
      say "  $label did not come online. Run the same command again to retry."
      exit 1
    fi
    sleep 5
  done
}

# Keep it simple: most people have Docker or nothing. Use a running engine if
# there is one, use Docker Desktop if it is installed, otherwise install and use
# OrbStack.
if engine_up; then
  ok "Using the container engine that is already running."
elif [ -d "/Applications/Docker.app" ]; then
  say "  Starting Docker Desktop. Accept any window it shows."
  open -a Docker || true
  wait_for_engine "Docker Desktop"
else
  if ! [ -d "/Applications/OrbStack.app" ]; then
    say "  Installing OrbStack."
    wait_note
    brew install --cask orbstack
  fi
  say "  Starting OrbStack. Accept any window it shows."
  open -a OrbStack || true
  wait_for_engine "OrbStack"
fi
ok "App engine is running."

# Safety net for the one error she kept hitting: if "docker compose" does not
# resolve (a stray Homebrew docker CLI not finding the plugin), link a compose
# binary into the place the docker CLI looks.
if ! compose_up; then
  COMPOSE_BIN="$(command -v docker-compose 2>/dev/null || true)"
  if [ -n "$COMPOSE_BIN" ]; then
    mkdir -p "$HOME/.docker/cli-plugins"
    ln -sf "$COMPOSE_BIN" "$HOME/.docker/cli-plugins/docker-compose"
  fi
fi
if ! compose_up; then
  say "  Could not enable 'docker compose'. Run the same command again to retry."
  exit 1
fi
ok "Compose is ready."

# ----------------------------------------------------------------------------
step 4 "Downloading Sift"
# ----------------------------------------------------------------------------
# If this is run from inside an existing checkout, use it. Otherwise clone.
if EXISTING_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" \
   && [ -f "$EXISTING_ROOT/docker-compose.yml" ] \
   && [ -f "$EXISTING_ROOT/package.json" ]; then
  INSTALL_DIR="$EXISTING_ROOT"
  ok "Using the copy you already have at $INSTALL_DIR."
elif [ -d "$INSTALL_DIR/.git" ]; then
  say "  Updating your existing copy."
  git -C "$INSTALL_DIR" pull --ff-only || true
  ok "Up to date at $INSTALL_DIR."
else
  say "  Downloading to $INSTALL_DIR."
  git clone "$REPO_URL" "$INSTALL_DIR"
  ok "Downloaded."
fi

cd "$INSTALL_DIR"

# ----------------------------------------------------------------------------
step 5 "Setting up your files"
# ----------------------------------------------------------------------------
if [ -f .env.example ]; then
  cp -n .env.example .env || true
fi
SETUP_QUIET=1 sh scripts/setup.sh || true
ok "Files ready."

# ----------------------------------------------------------------------------
step 6 "Starting the dashboard"
# ----------------------------------------------------------------------------
say "  Building and starting. The first build is the slow part."
wait_note
docker compose up -d --build

# Wait for the dashboard to answer before opening the browser.
PORT="${DASHBOARD_PORT:-3131}"
say "  Waiting for the dashboard to come online."
ATTEMPTS=0
until curl -fsS "http://localhost:${PORT}/healthz" >/dev/null 2>&1; do
  ATTEMPTS=$((ATTEMPTS + 1))
  if [ "$ATTEMPTS" -gt 120 ]; then
    say "  It is taking longer than expected. Opening the page anyway."
    break
  fi
  sleep 2
done

open "http://localhost:${PORT}" || true

# Clear the error trap so a clean finish does not prompt.
trap - ERR

if [ "$MODE" = "update" ]; then FINALE="Updated."; else FINALE="All set."; fi
cat <<DONE

========================================
  ${FINALE}
========================================

Your dashboard is open at http://localhost:${PORT}
If the page did not open, type that address into your browser.

A setup wizard in the page walks you through your free Gemini key
and your resume. Get the free key here (no credit card needed):
  https://aistudio.google.com/apikey

You can close this window now.
DONE

pause "Press Return to close this window."
