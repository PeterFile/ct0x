import { useEffect, useRef } from "react";

// Reuse existing reads only after the previous round settles; mutations keep their own ownership.
export function useActiveRefresh(active: boolean, pending: boolean, refresh: () => void) {
  const latest = useRef(refresh);
  useEffect(() => { latest.current = refresh; }, [refresh]);
  useEffect(() => {
    if (!active || pending) return;
    const timer = setTimeout(() => latest.current(), 5000);
    return () => clearTimeout(timer);
  }, [active, pending]);
}
