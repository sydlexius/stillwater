/* Per-screen page init.
 *
 * Each screen page sets `window.__screen` to the component name to render
 * (e.g. "DashboardProposal", "SettingsProposal", etc.) and includes its
 * required component scripts. This script wires it into #root with a
 * standard set of props and a small typography theme apply.
 *
 * Tweaks panel intentionally NOT included on these pages — keeps each
 * screen feeling like a real app screen, not a review surface.
 */
(function () {
  function App() {
    const screen = window.__screen;
    const Component = window[screen];

    if (!Component) {
      return React.createElement("div", { style: { padding: 40, color: "#cbd5e1", fontFamily: "system-ui" } },
        React.createElement("h1", null, "Component not found: " + screen),
        React.createElement("p", null, "Make sure window.__screen is set and the component script is loaded."));
    }

    return React.createElement(React.Fragment, null,
      React.createElement(Component, {
        density: "comfy",
        layout: window.__layout || "rail",
        showAnnotations: false,
      }),
      // Global ⌘K command palette — hidden until activated.
      window.CommandPaletteHost && React.createElement(window.CommandPaletteHost)
    );
  }

  const rootEl = document.getElementById("root");
  if (!rootEl) return;
  ReactDOM.createRoot(rootEl).render(React.createElement(App));
})();
