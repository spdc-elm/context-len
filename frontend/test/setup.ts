import { afterEach } from "vitest";

// React 19 checks this flag before reporting updates that happen from async
// effects.  Tests already use `act`; enabling the flag keeps the output useful
// instead of emitting the generic environment warning.
(globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

afterEach(() => {
  // Keep browser globals deterministic for future DOM/component tests.
  document.body.innerHTML = "";
});
