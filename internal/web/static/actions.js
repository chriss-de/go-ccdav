// Action panels for ccdav's web UI. Turns the <details> inside every
// [data-actions] element into a row of buttons, with at most one panel
// open at a time. Without JavaScript the <details> work on their own.
(function () {
  "use strict";

  function enhance(box) {
    var panels = Array.prototype.filter.call(box.children, function (el) {
      return el.tagName === "DETAILS";
    });
    if (!panels.length) return;
    var row = document.createElement("div");
    row.className = "tabs";
    var buttons = panels.map(function (d) {
      var b = document.createElement("button");
      b.type = "button";
      b.textContent = d.querySelector("summary").textContent;
      if (d.hasAttribute("data-danger")) b.className = "danger";
      b.addEventListener("click", function () {
        var open = !d.open;
        panels.forEach(function (p, i) {
          p.open = open && p === d;
          buttons[i].setAttribute("aria-expanded", String(p.open));
        });
      });
      b.setAttribute("aria-expanded", String(d.open));
      row.appendChild(b);
      return b;
    });
    box.insertBefore(row, box.firstChild);
    box.classList.add("tabbed");
  }

  Array.prototype.forEach.call(document.querySelectorAll("[data-actions]"), enhance);
})();
