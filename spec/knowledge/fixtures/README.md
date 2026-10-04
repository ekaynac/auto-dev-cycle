# Fixtures

- [`definitions.yaml`](definitions.yaml): fixture **definitions**: the inputs, the act and the expected outcome of
  each case, the invariants it exercises and the milestone whose behaviour implements its check. No check runs in
  M0; each arrives with its milestone (M1, M2 or M4), and an implementation passes a fixture when its behaviour
  produces the expected outcome.
- [`golden.json`](golden.json): golden digest vectors for refs and view fingerprints (inputs as given, canonical
  preimages, digests, and the empty-input case), computed independently of any implementation. A second
  implementation must reproduce them from the canonical-form rules in [`../vocabulary.md`](../vocabulary.md).

`internal/knowledge` reads both in its tests: the golden vectors pin its digests, and the definitions are checked for
shape (unique ids, a milestone of M1, M2 or M4, and invariants that exist).
