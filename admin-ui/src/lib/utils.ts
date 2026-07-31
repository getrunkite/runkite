import { clsx, type ClassValue } from "clsx";
import { twMerge } from "tailwind-merge";

/** Merges Tailwind classes, resolving conflicts (e.g. a later "px-4"
 * winning over an earlier "px-2") instead of leaving both in the
 * className string -- the standard shadcn/ui utility, needed anywhere a
 * component accepts a `className` prop to override/extend its defaults. */
export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}
