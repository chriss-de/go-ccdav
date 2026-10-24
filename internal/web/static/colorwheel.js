// Color wheel for ccdav's web UI. Enhances every [data-cw] element that
// contains a hex text input: adds a swatch button that opens a hue /
// saturation wheel with a brightness slider. Without JavaScript the hex
// input keeps working on its own.
(function () {
  "use strict";
  var SIZE = 200;

  function clamp(x, a, b) { return Math.min(b, Math.max(a, x)); }

  function hsvToRgb(h, s, v) {
    var i = Math.floor(h / 60) % 6, f = h / 60 - Math.floor(h / 60);
    var p = v * (1 - s), q = v * (1 - f * s), t = v * (1 - (1 - f) * s);
    var rgb = [[v, t, p], [q, v, p], [p, v, t], [p, q, v], [t, p, v], [v, p, q]][i];
    return rgb.map(function (c) { return Math.round(c * 255); });
  }

  function rgbToHsv(r, g, b) {
    r /= 255; g /= 255; b /= 255;
    var max = Math.max(r, g, b), min = Math.min(r, g, b), d = max - min, h = 0;
    if (d) {
      if (max === r) h = ((g - b) / d) % 6;
      else if (max === g) h = (b - r) / d + 2;
      else h = (r - g) / d + 4;
      h = (h * 60 + 360) % 360;
    }
    return [h, max ? d / max : 0, max];
  }

  function toHex(rgb) {
    return "#" + rgb.map(function (c) { return ("0" + c.toString(16)).slice(-2); }).join("");
  }

  function parseHex(s) {
    var m = /^#?([0-9a-f]{6})([0-9a-f]{2})?$/i.exec((s || "").trim());
    if (!m) return null;
    var n = parseInt(m[1], 16);
    return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
  }

  // The wheel image is the same for every picker (full brightness); the
  // brightness slider darkens it with an overlay.
  var wheelImage = null;
  function drawWheel(ctx) {
    if (!wheelImage) {
      wheelImage = ctx.createImageData(SIZE, SIZE);
      var R = SIZE / 2, d = wheelImage.data;
      for (var y = 0; y < SIZE; y++) {
        for (var x = 0; x < SIZE; x++) {
          var dx = x - R + 0.5, dy = y - R + 0.5, r = Math.sqrt(dx * dx + dy * dy), o = (y * SIZE + x) * 4;
          if (r > R) { d[o + 3] = 0; continue; }
          var h = (Math.atan2(dy, dx) * 180 / Math.PI + 360) % 360;
          var rgb = hsvToRgb(h, r / R, 1);
          d[o] = rgb[0]; d[o + 1] = rgb[1]; d[o + 2] = rgb[2];
          d[o + 3] = r > R - 1 ? Math.round((R - r) * 255) : 255; // soft edge
        }
      }
    }
    ctx.putImageData(wheelImage, 0, 0);
  }

  var openPicker = null;

  function enhance(root) {
    var input = root.querySelector("input[type=text]");
    if (!input) return;
    var hsv = rgbToHsv.apply(null, parseHex(input.value) || [58, 135, 173]);

    var swatch = document.createElement("button");
    swatch.type = "button";
    swatch.className = "cw-swatch";
    swatch.setAttribute("aria-label", "Pick a color");

    var pop = document.createElement("div");
    pop.className = "cw-pop";
    pop.hidden = true;

    var wrap = document.createElement("div");
    wrap.className = "cw-wheel";
    var canvas = document.createElement("canvas");
    canvas.width = canvas.height = SIZE;
    var shade = document.createElement("div");
    shade.className = "cw-shade";
    var knob = document.createElement("div");
    knob.className = "cw-knob";
    wrap.append(canvas, shade, knob);

    var slider = document.createElement("input");
    slider.type = "range";
    slider.min = 0; slider.max = 100;
    slider.className = "cw-value";
    slider.setAttribute("aria-label", "Brightness");

    pop.append(wrap, slider);
    root.insertBefore(swatch, input);
    root.append(pop);
    drawWheel(canvas.getContext("2d"));

    function render(fromInput) {
      var hex = toHex(hsvToRgb(hsv[0], hsv[1], hsv[2]));
      if (!fromInput) input.value = hex;
      swatch.style.background = hex;
      shade.style.opacity = String(1 - hsv[2]);
      slider.value = Math.round(hsv[2] * 100);
      slider.style.background = "linear-gradient(to right, #000, " + toHex(hsvToRgb(hsv[0], hsv[1], 1)) + ")";
      var R = SIZE / 2, a = hsv[0] * Math.PI / 180;
      knob.style.left = (R + Math.cos(a) * hsv[1] * R) + "px";
      knob.style.top = (R + Math.sin(a) * hsv[1] * R) + "px";
      knob.style.background = hex;
    }

    function pick(ev) {
      var rect = canvas.getBoundingClientRect(), R = rect.width / 2;
      var dx = ev.clientX - rect.left - R, dy = ev.clientY - rect.top - R;
      hsv[0] = (Math.atan2(dy, dx) * 180 / Math.PI + 360) % 360;
      hsv[1] = clamp(Math.sqrt(dx * dx + dy * dy) / R, 0, 1);
      if (hsv[2] < 0.05) hsv[2] = 1; // picking on a black wheel would do nothing
      render(false);
    }

    wrap.addEventListener("pointerdown", function (ev) {
      ev.preventDefault();
      wrap.setPointerCapture(ev.pointerId);
      pick(ev);
    });
    wrap.addEventListener("pointermove", function (ev) {
      if (wrap.hasPointerCapture(ev.pointerId)) pick(ev);
    });
    slider.addEventListener("input", function () {
      hsv[2] = slider.value / 100;
      render(false);
    });
    input.addEventListener("input", function () {
      var rgb = parseHex(input.value);
      if (rgb) { hsv = rgbToHsv.apply(null, rgb); render(true); }
    });
    swatch.addEventListener("click", function () {
      var show = pop.hidden;
      if (openPicker && openPicker !== pop) openPicker.hidden = true;
      pop.hidden = !show;
      openPicker = show ? pop : null;
    });
    render(true);
  }

  document.addEventListener("click", function (ev) {
    if (openPicker && !openPicker.parentNode.contains(ev.target)) {
      openPicker.hidden = true;
      openPicker = null;
    }
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && openPicker) { openPicker.hidden = true; openPicker = null; }
  });
  document.querySelectorAll("[data-cw]").forEach(enhance);
})();
