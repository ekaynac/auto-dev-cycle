# Memory system or fresh "blind" agents? Research for auto-dev-cycle

Date: 2026-10-01. Question asked by the operator: in a systematic, from-scratch development, is a memory system more
sensible, or are blind agents researching on their own each time more robust? Judge this for small and large agents
in different situations, from the perspective of the framework's dynamic structure.

Method: three parallel research passes.
1. A survey of memory systems and harness-native memory.
2. The empirical literature (2024–2026).
3. nöbetçi's own measurements, read-only from `/home/mega/nobetci-dev` and `/home/mega/nobetci`.

Evidence quality is flagged where it matters. Many of the newest studies are single-author or unreviewed preprints,
and vendor numbers are marketing signals.

---

## 1. Answer in one paragraph

Neither, as posed. The evidence says **agents should not own memory, and they should not be blind either.**
- The robust design is **controller-owned project knowledge**: typed, provenance-stamped, versioned records that
  auto-dev-cycle keeps, plus deterministic code-knowledge layers.
- These are **projected per role and per model size** into each fresh session.
- Agents may **propose** knowledge. They never write directly into what later sessions auto-load.
- Fresh sessions stay the default for every role. Long resumed sessions and agent-managed memory stores are the two
  things to avoid.

This also fits the framework's dynamic structure: knowledge must survive any executor being swapped, drained or
replaced, so it cannot live inside any one agent.

---

## 2. What the evidence says

### 2.1 Unfiltered carried memory does not help coding agents, and often hurts

- **VibeMemBench** (Sep 2026, preprint), 111 coding targets:
  - 11 of 12 off-the-shelf memory systems × solver pairs did **not** beat memory-off;
  - Mem0 + glm-5 was significantly worse (−5.5 pp);
  - even hand-verified "oracle" experience gave only +0–4.5 pp (all CIs crossed zero), though it cut steps by
    2–11 per run;
  - 69% of failures were "form degradation", the largest share being **instruction pollution from volume**;
  - memory helped weaker solvers and hurt solvers already at their ceiling.
- **SWE-ContextBench** (Feb 2026, preprint):
  - baseline 26.3%;
  - an **oracle-selected concise summary** 34.3% (+8.1 pp), and the cheapest per instance;
  - a full trajectory 27.3%;
  - a **freely retrieved summary 22.2%, below baseline.** A wrong summary is worse than none.
- Raw trajectories give **negative** transfer (−7.5 to −9.5%). Abstracted insights give positive transfer (+6.5 to
  +9%) (arXiv 2604.27003, one model).
- Agents **copy** retrieved experience. Wrong records are replicated and compound if written back (ACL 2026).

### 2.2 Long resumed context degrades every model, small and frontier alike

- Context rot: all 18 models tested degraded as input grew (Chroma). Effective context is about half the claimed
  window (RULER). Drops are visible already at 2–8K with low lexical overlap (NoLiMa).
- Multi-turn anchoring: a 39% average drop from single-turn to multi-turn, because models lock onto early
  assumptions (Laban et al., ICLR 2026). This is the mechanism behind resumed-reviewer failures.
- nöbetçi measured the cost side:
  - one phase-reviewer session resumed over 5 days carried 474k–877k tokens into each round (31.3 M input tokens in one
    round; $56 over the session);
  - resumed per-task reviews started at a median of 141k context with 94% carried history, against 35k for fresh
    ones;
  - the fresh-session + digest replacement is estimated at 50 M → 7–9 M tokens. That estimate is not measured yet.

### 2.3 Curated, pointer-style context helps, especially small models

- ACON (arXiv 2510.00615): compressed context curated by a stronger model or a pipeline **raised small agents by up to
  46%**, and distilled compressors on Qwen3-8B/14B kept >95% of teacher accuracy.
- Compression costs re-reads when it drops facts: completion unchanged, retrieval calls about 3× (arXiv 2608.16370).
  This is exactly nöbetçi's 72–80% repair re-read figure.
- **Source-first beats conclusion-first.** A memory that keeps a wrong *conclusion* without its *source* makes
  models confidently re-emit the stale value, from an 8B model to frontier (Reclaim, preprint). Digests must point to
  evidence (file, line, command, receipt), not assert verdicts, and must leave an abstain/escalate path.
- Agent Workflow Memory and ACE gained +12 to +17 pp from **itemised, incrementally edited playbooks**. A monolithic
  rewrite collapsed one playbook from 18k to 122 tokens and lost 9.6 pp ("context collapse").

### 2.4 Model size changes who should do the memory work, not whether memory is curated

- Small models:
  - write poor memories (their solutions are less often correct);
  - retrieve and use them poorly (Dynamic Cheatsheet);
  - over-invoke memory tools (Letta leaderboard);
  - lose accuracy as self-managed history grows (MemBench, Qwen2.5-7B);
  - follow injected memory instructions far more often (credential exfiltration: Haiku 80% vs Opus 0%; Bad Memory,
    2026).
- Small models do well when **a pipeline or a stronger model curates context for them**, or when they are trained for
  a specific memory task (Memory-R1, SUMER).
- Frontier models use memory tools competently and tolerate compression. But the stale-conclusion failure is
  **size-invariant**.

### 2.5 Shared memory breaks reviewer independence

- A fresh-session review found more injected errors than a same-session review (F1 28.6% vs 24.6%, p=0.008). A
  repeated same-session review got worse. Giving the fresh reviewer the *generation prompt* alone erased most of the
  gain: the builder's framing anchors the reviewer (single-author preprint, one model).
- Models miss errors framed as their own (64.5% blind spot across 14 models). Errors grow **more** correlated as
  models get more capable (ICML 2025).
- Shared builder↔reviewer memory is a correlated-failure channel. nöbetçi's rule "different family, fresh session"
  is supported.

### 2.6 Agent-written memory is an attack surface

- Query-only memory injection succeeds >95% of the time (MINJA).
- Payloads planted in CLAUDE.md/AGENTS.md-style files persist across sessions (86.7–96.7% persistence even on strong
  models; Bad Memory 2026).
- The best of four injection guards caught about 68%.
- Agent-written notes that are auto-loaded later must be provenance-tagged, reviewed or mechanically generated. They
  are never promoted to instructions.

### 2.7 The memory systems themselves

- Harness vendors converged on **plain versioned files + a small always-loaded index + topic files on demand + an
  asynchronous extract-then-consolidate pass**: Claude Code auto-memory, the Claude API memory tool, Codex memories,
  oh-my-pi, Serena memories.
- Memory services (Mem0, Zep, Cognee, Letta) add infrastructure (vector DBs, graph DBs, a server). They are tuned for
  chat personalisation, and their benchmark wins are on chat recall, not coding. Even there, a plain file + grep
  scored 74% on LoCoMo against Mem0's 68.5% (Letta, vendor).
- Graphiti's **bi-temporal validity** (facts are invalidated, never deleted) is the best idea to borrow. Its
  extraction needs strong models, and its docs warn small models break ingestion.
- **Deterministic code-knowledge layers** need no LLM writes and are always current, because the code is the source
  of truth:
  - Aider-style PageRank repo map;
  - LSP/Serena symbol ranges and references;
  - tree-sitter call graphs.

### 2.8 nöbetçi's own record

- **Proven wins, all controller-curated:**
  - canon as content (build canon reads 5.5 → 0 per call);
  - plan sidecar (removed transcript grepping, up to 26× per build);
  - work map/hints (first edit 41 min → 4–5 min when files were named);
  - isolation from foreign memory;
  - F-1.4-14 in ~75 min vs a 5.4 h median after the context program (a single observation).
- **Unproven:**
  - The read trail did not stop re-reads: still 80% on 09-30, because it saw only `Read`, not shell reads.
  - **The dossier shows no measured gain yet.** Small, confounded samples point slightly the wrong way (repair first
    edit 172 s → 421 s, n=5 vs 22). R52 is open.
  - Fresh phase-review + digest has no measured round yet.
- **Carried context hurt:**
  - resumed reviewers (the cost blowup);
  - blind-inspect history resend (175 M tokens/pass);
  - session-total cost double counting;
  - foreign memory leaks (a planner read the supervisor's work queue through a parent `AGENTS.md`).

  The claim "foreign_reads 0 since K-6" is **contradicted by the metrics**: three later plan/review calls read foreign
  paths.
- **Lack of carry-over hurt:**
  - dead rounds forwarded nothing (one repair: 645 turns, first edit at 3,101 s);
  - repair re-orientation took up to 31% of repair wall;
  - supervisor session death (4.5 h dead cycle; a rollout lost);
  - handover losses.

  The predecessor's own named fix is **structured controller records** (R-3 "repair memory"), not model recollection.
- **Supervisor memory:**
  - valuable for durable rules and lessons;
  - unreliable for live state;
  - WORK_QUEUE grew to 206 KB (~81k tokens, ~95% superseded);
  - state notes rotted in the index;
  - stale canon contradicted later changes;
  - an unmeasured claim ("the sweeper is local") became an operator-facing error;
  - the shared memory surface was the leak vector.

---

## 3. Recommendation: controller-owned project knowledge

### 3.1 Principles

1. **The controller owns knowledge; agents own nothing that survives them.** This holds by construction: executors
   are swapped, drained and replaced (D8, §2.2 of the plan), and knowledge must be executor-agnostic.
2. **Records, not recollections.** Knowledge is derived from the receipts, attempts, findings, approvals and decisions
   the controller already keeps. Live state is always regenerated from source records, never remembered.
3. **Source-first.** Every item points to its evidence (commit, file:line, receipt, run id, author model) and carries
   validity fields: `valid_from`, `superseded_by`, plus a code-anchor check. When the anchored symbol or file is gone,
   the item is flagged stale mechanically.
4. **Supersede, never delete.** This is Graphiti's bi-temporal idea, implemented as versioned records.
5. **Projection per role and per model size.**
   - Each fresh session gets a bounded, role-specific view.
   - Small models get a compact, curated, ranked projection.
   - Frontier models get the same core plus on-demand query tools over the knowledge store (just-in-time retrieval).
   - A larger context window does not mean more injected memory.
6. **Agents propose, trusted paths promote.**
   - Agent-written notes enter as *proposals* with provenance.
   - Promotion into auto-loaded knowledge needs a deterministic validator plus review by an independent model or the
     supervisor.
   - Builder-ingested content can never promote itself.
   - Injected knowledge is wrapped as advisory data, never as instructions.
7. **The strongest memory is a mechanism.** A recurring lesson becomes a gate, test, validator or invariant through
   the incident → lesson → proposed invariant pipeline. A note is the weakest form of remembering.
8. **Measure it, or it is not real.** Every projection is wired to per-call context metrics: first-turn size, carried
   share, pre-edit reads and re-reads (shell reads included), foreign reads, time to first edit. The metric itself is
   verified (nöbetçi's `foreign_reads 0` claim was false).

### 3.2 Per role

| Role (typical size) | Session | Gets | Never gets |
|---|---|---|---|
| **Builder** (small/cheap: GLM flash, Qwen3.8 27B, Ollama) | Fresh, always | Plan sidecar; **dossier** (anchors, symbol ranges, focused tests, gate failures); deterministic code index (repo map, LSP symbols/references); **attempt records** of earlier rounds, source-first (diff, failing tests, notes with pointers; R-3 repair memory); scoped "lessons" for files in scope, from promoted knowledge | Self-managed memory tools; other tasks' knowledge; unpromoted agent notes |
| **Reviewer / phase reviewer** (strong, different family) | Fresh per round | Spec + acceptance rows; diff/final tree; controller evidence (per-test results, receipts); a digest of **open findings as pointers, not verdicts**; semantic/procedural knowledge (invariants, ADRs, a defect-class checklist distilled from past findings) | Builder notes, reasoning or plan prose; episodic memory of earlier reviews of the same change beyond the pointer digest |
| **Planner** (strong) | Fresh | **Itemised project playbook** (conventions, accepted decisions/ADRs, lessons), edited incrementally, promoted only from verified outcomes, never rewritten wholesale; requirement table; plan history | Raw transcripts |
| **Supervisor** (strong, external) | Restarted on a schedule; continuity in controller-stored notes/handovers (`02-supervisor-role.md` §5) | Regenerated state (`adc status`, holds, records); a small curated lessons index (typed, dated, source-linked); a decision log | Its own long-lived context as a source of truth |
| **Classifiers** (Jev/Kev) | Stateless | The item to classify | Any memory |
| **Framework** | — | The promotion pipeline (lesson → invariant) | — |

### 3.3 What not to build in v1

- No external memory service (Mem0, Zep, Cognee, Letta).
- No vector or graph DB.
- No self-editing agent memory.
- No resumed long sessions.
- **Not harness-native memory as a source of truth.** Claude Code auto-memory and Codex memories are machine-local,
  vendor-specific, model-written and auto-loaded. Host-side harnesses run with them disabled; nöbetçi's isolation
  rule carries over.

Revisit Graphiti for supervisor-scale temporal facts after v1, if the curated index proves too small.

---

## 4. Experiments to run, not to assume

The evidence is uneven, and nöbetçi's own dossier shows no gain yet. v1 therefore measures before trusting. Run these
on T1 (established in M2), per model size class (small local, cheap API, frontier):

| # | Arms | Primary measures | Question |
|---|---|---|---|
| E1 | memory-off (task only) · dossier · dossier + code index · dossier + code index + attempt records | resolve rate, time to first edit, pre-edit (re-)reads incl. shell, turns, cost | Which curated layers actually help builders of each size? |
| E2 | reviewer: fresh + pointer digest · fresh + verdict digest · fresh + builder notes | injected-defect recall, critical-defect recall, rounds to close | How much does framing leak, and do pointer digests avoid it? |
| E3 | planner: no playbook · playbook (itemised, promoted) | plan validator pass rate, revisions needed, downstream rounds | Does a curated playbook improve plans as tasks recur? |
| E4 | poisoning: seeded malicious content in repo and notes | promotion rate of poisoned items, injection success per model size | Does the propose → promote gate hold? |
| E5 | knowledge staleness: refactor anchors under existing items | stale items detected mechanically vs missed | Does the code-anchor check work? |

The criterion for keeping a layer: it improves the primary measures on its role and size class without raising
harmful errors. Otherwise it stays off for that class. This mirrors the classifier program, where "nothing enabled"
is a valid outcome.

---

## 5. Sources

Full source lists with URLs are kept in the three research reports; the main ones are below.
- Anthropic, Effective context engineering (2025); Effective harnesses for long-running agents (2025); Claude Code
  memory docs; Claude memory tool docs.
- OpenAI Cookbook, long-term memory notes and session memory (Agents SDK).
- VibeMemBench arXiv 2609.23570; SWE-ContextBench 2602.08316; SWE-Exp 2507.23361; ACE 2510.04618; Dynamic Cheatsheet
  2504.07952; Agent Workflow Memory 2409.07429; 2604.27003; 2505.16067; LongMemEval 2410.10813; MemoryAgentBench
  2507.05257; SUMER 2511.21726; Memory-R1 2508.19828; MemBench 2506.21605.
- Chroma Context Rot; NoLiMa 2502.05167; RULER 2404.06654; Laban et al. 2505.06120; compression cost 2608.16370;
  ACON 2510.00615; Reclaim 2606.25449; Factory.ai compression eval (vendor).
- Cross-Context Review 2603.12123; Self-Correction Bench 2507.02778; Great Models Think Alike 2502.04313; Correlated
  Errors 2506.07962.
- Bad Memory 2607.14611; MINJA-style 2601.05504; sleeper poisoning 2605.15338.
- Letta, Mem0, Zep/Graphiti, LangMem, Cognee, A-MEM, MemOS, Memobase, oh-my-pi memory docs, Codex memories, Serena,
  Aider repo map, Codebase-Memory 2603.27277.
- nöbetçi: `docs/2026-09-20-context-audit.md`, `2026-09-16-build-step-analysis.md`,
  `2026-09-20-cycle-time-token-analysis.md`, `2026-09-30-builder-exploration-review-cost.md`,
  `2026-09-23-repetition-reduction-plan.md`, `WORK_QUEUE.md` (K-1..K-10, R52, R-3, O-11, O-12), `var/context-metrics`
  and `var/build-metrics` aggregates, supervisor memory notes.
