"""Terminal-Bench adapter: run Codex (GPT-5.5) or Claude Code (Opus 4.8) WITH the Entire
brain injected, against Terminal-Bench tasks, for a clean brain-vs-no-brain comparison.

STATUS: code-complete, NOT yet executed end-to-end. Running it requires (and is currently
blocked on):
  - OPENAI_API_KEY (for the Codex/GPT-5.5 arm) and/or ANTHROPIC_API_KEY (Claude/Opus arm)
    in the environment — Terminal-Bench's installed agents read these directly; the local
    subscription-authed `claude`/`codex` CLIs cannot substitute.
  - A linux/amd64 Docker runtime (Terminal-Bench task images are amd64). On Apple Silicon
    this means emulation (slow) or a cloud amd64 runner.

Design (grounded in terminal_bench 0.2.18 AbstractInstalledAgent):
  The brain arm = stock agent + Entire brain. We override perform_task to (1) copy a
  cross-compiled linux/amd64 entire-brain binary into the task container, (2) build
  seed+semantic on the task's working directory (Terminal-Bench tasks have NO Entire
  session history, so this is seed+semantic + the disciplined delivery packet only —
  the history half is proven separately on cli-bench, not here), (3) capture the brief
  packet, and (4) prepend the disciplined two-step delivery policy (the same
  verify-the-invariant shape shipped in run.py's full_cli_compact path — name the
  invariant from the brief, treat likely_edit_files as a candidate to VERIFY, not blindly
  trust). The no-brain control is the stock CodexAgent / ClaudeCodeAgent, so the A/B
  isolates the brain's contribution and reproduces the published baseline.

Register with:
  tb run --agent-import-path benchmarks.agent_brain.adapters.terminal_bench.entire_brain_agent:EntireBrainCodexAgent \
         --model gpt-5.5 --dataset-name terminal-bench-core ...
  (set ENTIRE_BRAIN_LINUX_BIN to the cross-compiled binary; GOOS=linux GOARCH=amd64 go build ./cmd/entire-brain)
"""

from __future__ import annotations

import os
import shlex
from pathlib import Path

from terminal_bench.agents.installed_agents.codex.codex_agent import CodexAgent
from terminal_bench.agents.installed_agents.claude_code.claude_code_agent import (
    ClaudeCodeAgent,
)
from terminal_bench.agents.base_agent import AgentResult
from terminal_bench.agents.failure_mode import FailureMode
from terminal_bench.terminal.tmux_session import TmuxSession

# The disciplined, self-correcting delivery packet — kept byte-aligned in intent with the
# full_cli_compact policy in benchmarks/agent-brain/run.py. The history half is absent on
# Terminal-Bench (fresh repos), so this leans on seed+semantic localization + the verify step.
_DISCIPLINED_PACKET = (
    "You have an Entire Brain for this repository. Before editing, run "
    "`entire-brain brief {query} --json` once. Work in this order: (1) read the brief's "
    "top hits and name the EXACT thing the task needs changed — the specific symbol, "
    "expression, or behavior; (2) treat `likely_edit_files[0]` as a CANDIDATE and VERIFY "
    "it actually contains that thing before editing; (3) if it does not, the evidence "
    "decides the file, not the ranking — check the next candidate or run at most ONE "
    "targeted search, then edit the file that truly contains it. Apply the minimal change, "
    "validate once, then stop. `likely_edit_files` is a hint that can be wrong; the brief's "
    "evidence is the authority. Avoid broad repeated repo-wide search.\n\n"
)


class _BrainInjectionMixin:
    """Shared brain-injection: copy the linux binary in, build seed+semantic on the repo,
    and prepend the disciplined packet to the instruction. Override points only."""

    BRAIN_CONTAINER_PATH = "/installed-agent/entire-brain"

    def _host_brain_binary(self) -> Path:
        raw = os.environ.get("ENTIRE_BRAIN_LINUX_BIN")
        if not raw:
            raise RuntimeError(
                "ENTIRE_BRAIN_LINUX_BIN is not set — point it at a linux/amd64 entire-brain "
                "binary (GOOS=linux GOARCH=amd64 go build -o <path> ./cmd/entire-brain)."
            )
        path = Path(raw)
        if not path.is_file():
            raise RuntimeError(f"ENTIRE_BRAIN_LINUX_BIN does not exist: {path}")
        return path

    def _detect_repo_dir(self, session: TmuxSession) -> str:
        # Terminal-Bench tasks set their own workdir; the container shell starts there.
        result = session.container.exec_run(["sh", "-c", "pwd"])
        out = result.output.decode().strip() if hasattr(result, "output") else "/app"
        return out.splitlines()[-1] if out else "/app"

    def _build_brain(self, session: TmuxSession, repo_dir: str) -> None:
        binary = self._host_brain_binary()
        session.copy_to_container(
            binary, container_dir="/installed-agent", container_filename="entire-brain"
        )
        session.container.exec_run(["chmod", "+x", self.BRAIN_CONTAINER_PATH])
        # seed + semantic only — no history exists on a Terminal-Bench repo.
        session.container.exec_run(
            [self.BRAIN_CONTAINER_PATH, "refresh", "seed", repo_dir, "--agent", "none", "--force"]
        )
        session.container.exec_run(
            [self.BRAIN_CONTAINER_PATH, "refresh", "index", repo_dir, "--force"]
        )

    def _augment_instruction(self, instruction: str) -> str:
        query = shlex.quote(instruction[:160])
        return _DISCIPLINED_PACKET.format(query=query) + instruction

    def perform_task(
        self, instruction: str, session: TmuxSession, logging_dir: Path | None = None
    ) -> AgentResult:
        try:
            repo_dir = self._detect_repo_dir(session)
            self._build_brain(session, repo_dir)
        except Exception as exc:  # surface as a clean failure rather than a silent no-brain run
            return AgentResult(
                total_input_tokens=0,
                total_output_tokens=0,
                failure_mode=FailureMode.AGENT_INSTALLATION_FAILED,
                timestamped_markers=[(0.0, f"entire-brain injection failed: {exc}")],
            )
        return super().perform_task(self._augment_instruction(instruction), session, logging_dir)


class EntireBrainCodexAgent(_BrainInjectionMixin, CodexAgent):
    @staticmethod
    def name() -> str:
        return "entire-brain-codex"


class EntireBrainClaudeAgent(_BrainInjectionMixin, ClaudeCodeAgent):
    @staticmethod
    def name() -> str:
        return "entire-brain-claude-code"
