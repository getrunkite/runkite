import { createContext, useContext } from "react";
import type { ReviewSnapshot } from "./contracts";

export const ReviewContext = createContext<(ReviewSnapshot & { error: string | null; retry: () => void }) | null>(null);
export function useReview() {
  const context = useContext(ReviewContext);
  if (!context) throw new Error("Review context is required.");
  return context;
}
