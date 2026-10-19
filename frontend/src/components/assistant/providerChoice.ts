// providerChoice holds the provider checks that the profile Model card and the
// admin Defaults card share.
import type { AssistantProvider } from "../../types/assistant";

/** savedProviderGone is true when the saved provider is not in the loaded chart provider list. */
export function savedProviderGone(saved: string, list: AssistantProvider[], loaded: boolean): boolean {
  return loaded && saved !== "" && !list.some((p) => p.name === saved);
}

/** PROVIDERS_FAILED words the notice for a provider list that did not load. */
export const PROVIDERS_FAILED = "The provider list did not load. Only the saved values and Default can be shown.";
