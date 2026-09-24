import { defineConfig, globalIgnores } from "eslint/config";
import nextVitals from "eslint-config-next/core-web-vitals";
import nextTs from "eslint-config-next/typescript";

const eslintConfig = defineConfig([
  ...nextVitals,
  ...nextTs,
  globalIgnores([".next/**", "out/**", "build/**", "next-env.d.ts"]),
  {
    // SidebarMenuSkeleton picks a random width in a useMemo. The file is the
    // unforked registry primitive, so the rule is silenced here rather than
    // the component edited.
    files: ["src/components/ui/sidebar.tsx"],
    rules: { "react-hooks/purity": "off" },
  },
]);

export default eslintConfig;
