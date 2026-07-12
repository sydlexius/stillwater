// probe-helpers.js - browser-side utilities shared by every probe in
// probes.js. Installed once per page via installProbeHelpers(page) (an
// addInitScript, so window.__swDescribeSelector survives navigations within
// the same page/context).

export async function installProbeHelpers(page) {
  await page.addInitScript(() => {
    // Builds a short, human-readable selector for an offending element:
    // prefers #id, otherwise walks up to 4 ancestors joining tag + first 2
    // classes + :nth-of-type when siblings share a tag. Not guaranteed
    // unique -- it is diagnostic output for a report, not a re-query key.
    window.__swDescribeSelector = function swDescribeSelector(el) {
      if (el.id) return `#${el.id}`;
      const path = [];
      let node = el;
      let depth = 0;
      while (node && node.nodeType === 1 && depth < 4) {
        let seg = node.tagName.toLowerCase();
        if (node.classList && node.classList.length) {
          seg += '.' + Array.from(node.classList).slice(0, 2).join('.');
        }
        const parent = node.parentElement;
        if (parent) {
          const siblings = Array.from(parent.children).filter(c => c.tagName === node.tagName);
          if (siblings.length > 1) {
            seg += `:nth-of-type(${siblings.indexOf(node) + 1})`;
          }
        }
        path.unshift(seg);
        if (node.id) {
          path[0] = `#${node.id}`;
          break;
        }
        node = parent;
        depth++;
      }
      return path.join(' > ');
    };
  });
}
