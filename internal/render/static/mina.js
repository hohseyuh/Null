// Al-Mina keyboard layer. The page works without this file (each row has
// plain radio buttons and one submit); this only makes a batch of fifteen
// decisions fifteen keystrokes instead of fifteen clicks. It changes
// nothing the server would not accept from the bare form.
(() => {
  "use strict";
  const form = document.getElementById("minaform");
  if (!form) return;
  const rows = [...form.querySelectorAll("li.entry.proposal")];
  if (rows.length === 0) return;
  let cur = 0;

  const radios = (row) => [...row.querySelectorAll('input[type="radio"]')];
  function paint() {
    rows.forEach((r, i) => r.classList.toggle("focus", i === cur));
    rows.forEach((r) => {
      const picked = radios(r).find((x) => x.checked);
      r.classList.remove("decided-approve", "decided-deny", "decided-defer");
      if (picked && picked.value !== "skip") r.classList.add("decided-" + picked.value);
    });
    rows[cur].scrollIntoView({ block: "nearest" });
  }
  function choose(value, advance) {
    const r = radios(rows[cur]).find((x) => x.value === value);
    if (r) r.checked = true;
    if (value === "deny") { rows[cur].querySelector(".denyreason").focus(); paint(); return; }
    if (advance && cur < rows.length - 1) cur++;
    paint();
  }

  document.addEventListener("keydown", (ev) => {
    if (ev.ctrlKey || ev.metaKey || ev.altKey) return;
    const typing = ev.target instanceof HTMLInputElement && ev.target.type === "text";
    if (typing) {
      // in a denial reason: Enter/Escape return to the queue
      if (ev.key === "Enter" || ev.key === "Escape") {
        ev.preventDefault(); ev.target.blur();
        if (cur < rows.length - 1) cur++;
        paint();
      }
      return;
    }
    switch (ev.key) {
      case "j": case "ArrowDown": ev.preventDefault(); cur = Math.min(rows.length - 1, cur + 1); paint(); break;
      case "k": case "ArrowUp": ev.preventDefault(); cur = Math.max(0, cur - 1); paint(); break;
      case "a": choose("approve", true); break;
      case "d": ev.preventDefault(); choose("deny", true); break;
      case "f": choose("defer", true); break;
      case "s": choose("skip", true); break;
      case "Enter": ev.preventDefault(); form.requestSubmit(); break;
    }
  });
  form.addEventListener("change", (ev) => {
    const row = ev.target.closest("li.entry");
    if (row && rows.includes(row)) cur = rows.indexOf(row);
    paint();
  });
  paint();
})();
