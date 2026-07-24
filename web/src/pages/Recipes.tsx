import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { CodeBlock } from "../components/DocBlocks";
import {
  RECIPES,
  RECIPE_CATEGORIES,
  FeatureChips,
  findRecipe,
} from "../recipes";

function RecipesIndex() {
  const [filter, setFilter] = useState("");
  const q = filter.trim().toLowerCase();

  const matches = RECIPES.filter(
    (r) =>
      q === "" ||
      r.title.toLowerCase().includes(q) ||
      r.summary.toLowerCase().includes(q) ||
      r.category.toLowerCase().includes(q) ||
      r.features.some((f) => f.toLowerCase().includes(q))
  );

  return (
    <>
      <div className="recipes-head">
        <h1>Recipes &amp; use cases</h1>
        <p className="muted">
          Real-world CI/CD pipelines you can copy into a repo config today. Every{" "}
          <code>.forge-ci.yml</code> here compiles on this Forge server — pick a
          scenario, read how it maps to Forge, and copy the YAML.
        </p>
        <input
          className="recipes-filter"
          placeholder="Filter recipes by name, feature, or category…"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
        />
      </div>

      {RECIPE_CATEGORIES.map((cat) => {
        const items = matches.filter((r) => r.category === cat);
        if (items.length === 0) return null;
        return (
          <section key={cat} className="recipes-cat">
            <h2 className="recipes-cat-title">{cat}</h2>
            <div className="recipes-grid">
              {items.map((r) => (
                <Link
                  key={r.slug}
                  to={`/recipes/${r.slug}`}
                  className="glass recipe-card"
                >
                  <div className="recipe-card-diagram">{r.diagram()}</div>
                  <div className="recipe-card-title">{r.title}</div>
                  <div className="recipe-card-summary muted">{r.summary}</div>
                  <FeatureChips features={r.features.slice(0, 3)} />
                </Link>
              ))}
            </div>
          </section>
        );
      })}

      {matches.length === 0 && (
        <p className="muted">No recipes match “{filter}”.</p>
      )}
    </>
  );
}

function RecipeDetail({ slug }: { slug: string }) {
  const recipe = findRecipe(slug);
  if (!recipe) {
    return (
      <div className="docs-content">
        <h1>Recipe not found</h1>
        <p>
          No recipe with slug <code>{slug}</code>.{" "}
          <Link to="/recipes">Back to all recipes</Link>.
        </p>
      </div>
    );
  }

  return (
    <article className="docs-content recipe-detail">
      <div className="docs-crumb muted">
        <Link to="/recipes">Recipes</Link> <span className="crumb-sep">/</span>{" "}
        {recipe.category}
      </div>
      <h1>{recipe.title}</h1>
      <p className="recipe-detail-summary">{recipe.summary}</p>

      <div className="recipe-detail-diagram">{recipe.diagram()}</div>

      <h2>Features used</h2>
      <FeatureChips features={recipe.features} />

      <h2>Scenario</h2>
      {recipe.scenario()}

      <h2>
        <code>.forge-ci.yml</code>
      </h2>
      <CodeBlock code={recipe.yaml} />

      <p className="muted recipe-detail-foot">
        Paste this into repo <strong>Settings → Pipeline config</strong> (or PUT
        it to <code>/api/v1/repo-configs</code>). See{" "}
        <Link to="/docs/writing-yaml">Writing pipeline YAML</Link> for the full
        DSL reference.
      </p>
    </article>
  );
}

export default function Recipes() {
  const { slug } = useParams<{ slug: string }>();
  return (
    <div className="recipes-page">
      {slug ? <RecipeDetail slug={slug} /> : <RecipesIndex />}
    </div>
  );
}
