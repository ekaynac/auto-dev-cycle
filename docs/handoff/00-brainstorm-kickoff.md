# auto-dev-cycle → first open-source release: brainstorming kickoff

Prepared 2026-10-01 by the current supervisor session (Claude Opus 5.5), which has run the framework
unattended (codename **nöbetçi**) while it developed one real product, InfraMedic.
This document starts a brainstorming session. Its result is a plan for the **first release** of
auto-dev-cycle as an open-source, project-agnostic platform.
The other files in this folder carry the know-how:

| File | What it gives you |
|---|---|
| `00-brainstorm-kickoff.md` (this) | the operator's intent, the vision with examples, constraints, open questions, a proposed brainstorming agenda |
| `01-architecture-and-components.md` | what exists today: tiers, pipeline, roles, every model surface, isolation, evidence, ops, all product/GitHub coupling points |
| `02-lessons-incidents-invariants.md` | what three weeks of unattended operation taught us: invariants, incident classes and fixes, numbers, operator decisions |
| `03-planning-reviews-roadmap.md` | the planning head and task/canon model, reviews of the system, every design doc (current vs superseded), existing future plans, a reading order |

Source repo of the current framework: `/home/mega/nobetci-dev` (git, branch `main`). Its runtime is
`/home/mega/nobetci`, and its root-owned sandbox is `/srv/nobetci`. **Do not modify the runtime or the
current source tree.** The current session keeps supervising them until InfraMedic Phase 1 is finished.
Build the release in a new repo or branch.

---

## 1. Where we are (one paragraph)

auto-dev-cycle takes tasks from a queue and runs each through a fixed pipeline:

`plan → isolated build → canon/acceptance guard → full test gate → independent review → repair rounds → merge → deploy → deterministic ledger`

It also runs a phase/subphase audit gate. It then delivers the result as a merged PR plus a deploy, with no human in the loop.
In about 48 hours (2026-09-30/10-01) it delivered about 15 product tasks unattended. After the review gate was
simplified, the build and review steps have run almost without manual intervention.

The hard-won lessons are about three things: keeping a long-running loop alive (deadlock classes, provider failures, install hazards),
keeping cost bounded (resent context, over-engineered review gates), and evidence integrity (never a fabricated pass).
The operator's verdict: the review step had become too complex and token-hungry, and returning to a
simple gate was right. The framework is still tailored to one product, GitHub and one machine.
That tailoring is what the release must remove.

## 2. The operator's intent (verbatim intent, translated; original Turkish in Appendix A)

- **Open source.** Publish the framework fully open-source.
- **One routing layer for everything.** Manage every LLM call *and every harness* (Claude Code, Codex,
  opencode/OpenClaw-like CLIs, raw API calls, local servers) through **one routing layer**. Development
  can then continuously use token-usage rights from *any* source:
  - free usage tiers offered on the internet;
  - subscription quotas;
  - paid APIs;
  - local GPUs.
  New and more advanced harnesses can be plugged in.
- **Framework/project agnostic.** A platform that can do any kind of software development, not only
  InfraMedic, and not only Python/pytest.
- **Use idle capacity at full power.** Labs and enterprises with local LLM access can use their
  hardware fully during low-workload hours. Example: a university computer lab with **20 PCs, each with an RTX 5090**.
  At night, the idle PCs run **Qwen3.8 27B NVFP4** and connect to auto-dev-cycle. So auto-dev-cycle must
  **route model usage itself** and have a **plan to keep idle resources continuously busy**.
  Reference for the routing idea: <https://github.com/openfreerouter/freerouter> ("something like this").
- **Classifier models on many surfaces.** The operator sees many surfaces where "jev" or similar
  classifier models can be used. (The operator's term; the brainstorm should pin down what it means
  and where it applies.)
- **Generalise the P3-sweeper pattern.** The sweeper already does low-risk P3 work on spare/local
  capacity. The same pattern should exist on **every surface that uses a model**.
- **A planning head native to auto-dev-cycle.** So far we followed a plan and canon that were *not* written
  for auto-dev-cycle (the InfraMedic roadmap). The release needs a head that **produces plans
  suited to auto-dev-cycle**.
  - It is **capacity-aware**: at plan time it evaluates the capacity available *right now* and splits work into
    sub-plans for sub-agents to match.
  - The classifier models can be used here too.
  - The goal is an **abstract network of LLMs** that evaluates its own capacity dynamically and automatically.
- **Not only GitHub.** Today everything goes through GitHub (`gh`, PRs). The release must work over
  **plain git, GitLab and GitHub**.
- **Division of labour.** The current session keeps developing InfraMedic on the current
  cycle and finishes Phase 1 with only necessary changes, while it collects know-how. The new session
  owns the release.

## 3. Examples to anchor the brainstorm

1. **Night lab.** 20 lab PCs (RTX 5090 each) register as workers from 22:00 to 07:00 and serve a local
   Qwen 27B NVFP4.
   - The router sends them the work they handle well: builds of small, well-anchored tasks, P3
     sweeps, test-writing, dossier and context preparation, classification and triage. Higher-stakes work
     (independent review, planning, phase gates) goes to stronger remote models.
   - At 07:00 the workers drain gracefully. In-flight work is checkpointed (notes, trail, partial candidate)
     and resumes elsewhere, as the current "errored-build continuation" already does.
2. **Free-tier harvesting.** Several providers offer free or subscription quotas, such as the Claude Code subscription
   and Codex credits. Today the phase reviewer falls back from Codex/Astra to Opus when Codex hits its
   usage limit. Generalise that into a router that:
   - knows every quota and its reset time;
   - assigns work to whichever source has headroom;
   - never fails a task because one source is exhausted.
3. **Classifier surfaces** (candidates for "jev"-like small models):
   - routing a review finding to builder / closure-text / product. Today this is regex in `nb-fix-input`, and it caused two deadlocks.
   - classifying a provider error as transient or permanent. Today this is regex in `nb_transient.py`.
   - detecting degenerate or runaway builder output.
   - choosing which model or harness fits a task.
   - splitting a plan into parallelisable units.
   - deciding whether a closure text is consistent with the diff.
4. **Provider health from evidence.** On 2026-10-01 we proved from the event streams that one upstream
   (Relace) sends tool-call arguments in one chunk, which trips 120 s idle cuts on large writes. We then
   excluded it. A router should learn such facts automatically from per-turn telemetry (per-provider
   chunking, idle gaps, error rates) and adapt.
5. **Capacity-aware planning.** If 3 strong and 15 small workers are available, the planner emits 3
   high-judgement units and 15 mechanical units, each with a dossier (anchors, symbols, tests). If only one
   model is available, it emits a sequential plan. (The framework already has adaptive builds with up to 3
   sub-builders and a mechanical task dossier.)

## 4. Non-negotiables carried over (details in 02)

- **Evidence integrity.**
  - There are four outcomes: found / empty / could_not_run / failed. Something that couldn't be measured is never treated as clean.
  - Gate results are bound to the exact HEAD and tree.
  - Receipts must prove what they claim; for example, a lab-deploy receipt must show the candidate is actually running.
- **Isolation.** The builder runs in an isolated sandbox user with only the patch and its prompt. It cannot see credentials,
  and canon/protected paths are guarded.
- **Never silently lose work or money.**
  - Copy evidence before destroying a run.
  - Bounded retries.
  - Never reset counters or charges.
  - Per-call cost accounting.
- **Keep the loop alive.** A stopped cycle outranks everything. Deadlock classes must be designed out, not
  patched one by one (see the catalogue in 02).
- **Simple, meaningful gates.** The operator rejected an 8-step evidence gate and an obligation/blind-
  verification machinery as too complex and too expensive. Target $5–15 per subphase gate. Deterministic evidence is enough.
  Page a human only on real blockers.
- **Information up front, not read caps.** Repeated exploration is cured by handing over a dossier,
  notes and trail, never by capping what a model may read. Caps are safety rails, not budgets.
- **Independent review before installing framework changes.** Every framework fix in the current
  system went through an independent reviewer GO, and many were NO-GO first.

## 5. Open questions for the brainstorm (proposed agenda)

1. **Scope of v1.** What is the smallest release that is useful to others? Single machine or
   multi-node from day one? Which project types: any git repo with a test command? Which harnesses
   ship in v1?
2. **Routing layer architecture.** Build on or borrow from freerouter or LiteLLM-like gateways, or build our own?
   - How do we model sources (quota, reset, price, capability, trust, latency, streaming behaviour)?
   - How do we model harnesses as first-class routable executors? A harness is not just a model endpoint: it has tools, a sandbox and session semantics.
3. **Worker federation.** How do idle machines join and leave (lab PCs at night)?
   - Who handles security (untrusted worker nodes executing builds? data exposure?), scheduling, preemption and checkpoint/resume?
   - How do we stay within licences and terms of service when using free tiers?
4. **Classifier ("jev") layer.** Which decisions become classifier calls?
   - How are classifiers trained or evaluated, with fallback to deterministic rules?
   - How do we prevent a cheap classifier from creating a false pass?
5. **Planning head.** What does a framework-native plan look like? Phases, tasks, acceptance rows with ids, claims, dossiers.
   - How does it become capacity-aware?
   - How does it relate to a project's own roadmap/canon (import vs generate)?
   - How are phase gates defined generically?
6. **Forge abstraction.** git / GitLab / GitHub: PR/MR, review comments, CI, protected branches, deploy hooks.
7. **Deploy/evidence adapters.** InfraMedic-specific pieces (edge SSH deploy, lab-deploy, browser walk,
   ledger format) become plugins. What is the plugin contract?
8. **State, observability, ops.** Today's state is JSONL + JSON files plus systemd user units on one host.
   - What replaces or keeps them?
   - Dashboard, paging, the cost ledger.
9. **Licensing, naming, community.** Licence choice, repo layout, documentation, English-only codebase (today it mixes Turkish and English).
10. **Migration.** Can InfraMedic's later phases run on the new release? What is the dogfooding plan?

## 6. What to reuse vs rethink (starter list, refine in the brainstorm)

**Reuse (proven):**
- the cycle stage model and its terminal/step vocabulary;
- isolated builder worker;
- deterministic ledger;
- gate bound to HEAD and tree with per-test results;
- simple subphase gate (claims + receipts + one reviewer);
- closure-text call;
- errored-build continuation;
- task dossier, notes and trail;
- transient-retry classifier with bounded plans;
- hold watcher with staged installs at idle boundaries;
- the independent-review culture.

**Rethink:**
- model/harness routing (today it is scattered across roles.conf, provider env, per-role CLIs);
- the one-host systemd design;
- GitHub-only flow;
- product-named adapters in `bin/`;
- regex classifiers (fix-input routing, transient errors);
- the size of the installer/manifest machinery;
- the legacy obligation/blind/package code paths (superseded; candidates for removal);
- Turkish strings.

## Appendix A — operator's original message (Turkish, 2026-10-01)

> bir vizyona ihtiyacımız var artık. build stepleri neredeyse sorunsuz ilerleyebiliyor artık unattended şekilde. küçük küçük geliştirmeler yapıyoruz. review stepi çok karmaşıklaşmıştı şimdi eski haline getirerek iyi bir iş yaptık bence. içinde çıkılmaz bir haldeydik ve çok fazla token yiyordu. inframedic sen tarafından bu session supervisorlüğünde geliştirilmeye devam edilecek ve phase 1 bu şekilde bitirilecek. auto-dev-cycle ise artık demodan ve development'tan çıkarılmak üzere ayrı bir session tarafından ayrı bir repoda ya da branch'te geliştirilmeye başlanılacak. […] planım bu framworkün tamamiyle open-source bir şekilde yayınlanması. kullnılan ve kullanılacak bütün llm çağrılarını ve harness'ları tek bir routing katmanında yönetmek istiyorum. bu sayede development continious olarak her türlü kaynaktan gelebilecek token-usage haklarını değerlendirebilecek. yeni ve gelişmiş harness'lar bağlanılabilecek. framework agnostic olarak her türlü geliştirmeyi yapabilecek bir platform hayal ediyorum. güçlü yanlarından biri de lokal llm erişimi olan lab'ların ya da enterprise şirketlerin workload düşük olan saatlerde donanımlarını tam kapasite değerlendirmelerini sağlamak olacak. […] mesela 20 bilgisarlı herbirinde rtx 5090 olan bir üniversite bilgisayar labaratuvarında gece saatlerinde boş olan pclerde qwen3.8 27b nvfp4 çalıştırılarak auto-dev-cycle'a bağlanabilir. yani auto dev cycle model kullanımını da route etmeli, boştaki kaynakları sürekli kullanmak üzre bir planı olmalı. https://github.com/openfreerouter/freerouter bu github reposundaki gibi bir şeyler düşünülebilir. ayrıca auto dev cycle'ın üzerinde jev ya da benzeri classifier modelleri kullanabileceğimiz pek çok yüzey olduğunu düşünüyorum. temel amaçlarımızdan biri internet üzerindeki free usage olarak sunulan modelleri ve lokal model erişimi olan işletmelerdeki donanımları tam kapasiteyle boş bırakmadan kullanabilmek. p3 sweeper olarak geliştirdiğimiz gibi bu bütün model kullanılan yüzlerde kurgulanabilir. ayrıca şu ana kadar inframedic üzerinden, auto-dev-cycle için özel geliştirilmemiş bir planı ve kanonu takip ederek geliştirmeler yaptık. auto-dev-cycle'a uygun olacak bir plan geliştirebilecek bir head de tasarlamalıyız. dinamik olarak elimizdeki kapasiteyi otomatik değerlendirebilecek birsistem hayal ediyoruz. mesela plan aşamasında o an elimizde olan değerlendirilebilecek şekilde subagentlara ayrılmış planlar yapılabilir. bu yüzeyde de jev kullanılabilir. biz llm'leri kullanarak abstract bir ağ kurmaya çalışıyoruz. sistemi yalnızca github üzerinden kurguladık şu ana kadar ama hem git üzerinden gitlab üzerinden de yönetilebilmeli.
