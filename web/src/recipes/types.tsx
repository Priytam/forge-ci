import type { ReactElement, ReactNode } from "react";

/**
 * A single recipe / use-case entry. To add another recipe (toward 100), write a
 * new module under recipes/entries/ that exports a `Recipe` and append it to the
 * RECIPES array in recipes/index.tsx — nothing else needs to change.
 */
export interface Recipe {
  slug: string;
  title: string;
  /** grouping bucket shown on the index and in the sidebar */
  category: string;
  /** one-line summary for the card */
  summary: string;
  /** short chips naming the Forge features the recipe exercises */
  features: string[];
  /** original inline SVG pipeline diagram */
  diagram: () => ReactElement;
  /** prose scenario write-up (what / why / how it maps to Forge) */
  scenario: () => ReactNode;
  /** the full, compiler-validated .forge-ci.yml */
  yaml: string;
}

/** Feature chip row reused on cards and detail pages. */
export function FeatureChips({ features }: { features: string[] }): ReactElement {
  return (
    <div className="recipe-chips">
      {features.map((f) => (
        <span key={f} className="recipe-chip">
          {f}
        </span>
      ))}
    </div>
  );
}
