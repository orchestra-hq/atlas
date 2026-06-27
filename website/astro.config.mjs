// @ts-check
import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";
import starlightLinksValidator from "starlight-links-validator";

// Atlas docs site. Served from GitHub Pages at https://orchestra-hq.github.io/atlas
// (project page → base path "/atlas"). See docs/internal/m5-build-plan.md.
export default defineConfig({
  site: "https://orchestra-hq.github.io",
  base: "/atlas",
  integrations: [
    starlight({
      title: "Atlas",
      description:
        "Self-hosted LLM inference platform — point Claude Code and OpenAI SDKs at hardware you control.",
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/orchestra-hq/atlas",
        },
      ],
      editLink: {
        baseUrl: "https://github.com/orchestra-hq/atlas/edit/main/website/",
      },
      // Fails the build on broken internal links — the per-PR "link check" gate.
      plugins: [starlightLinksValidator()],
      sidebar: [
        { label: "Get started", items: [{ autogenerate: { directory: "get-started" } }] },
        { label: "Guides", items: [{ autogenerate: { directory: "guides" } }] },
        { label: "Deploy", items: [{ autogenerate: { directory: "deploy" } }] },
        { label: "Operate", items: [{ autogenerate: { directory: "operate" } }] },
        { label: "Reference", items: [{ autogenerate: { directory: "reference" } }] },
        { label: "About", items: [{ autogenerate: { directory: "about" } }] },
      ],
    }),
  ],
});
