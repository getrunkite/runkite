export type RegistryEntryMode = "create" | "edit";

// Keep the legacy create and detail URLs while giving every existing name,
// including the literal "new", an unambiguous edit route.
export const registryEntryRoutes = [
  { path: "registry/new", mode: "create" },
  { path: "registry/:name/edit", mode: "edit" },
  { path: "registry/:name", mode: "edit" },
] satisfies Array<{ path: string; mode: RegistryEntryMode }>;
