import type { LucideIcon } from "lucide-react";

export function NavIcon({ icon: Icon }: { icon: LucideIcon }) {
  return <Icon aria-hidden="true" focusable="false" size={18} strokeWidth={1.8} />;
}
