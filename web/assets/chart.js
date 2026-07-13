/* TryBerry — график истории цены (uPlot). Серия встроена в SSR (мгновенный рендер),
   переключение периода — XHR. Signature: опорные линии «минимум» и «обычная цена»,
   на фоне которых видно, выгодна ли текущая цена. */
(function () {
  "use strict";
  var boot = window.__CHART__ || {};
  var publicId = boot.publicId;
  var refs = boot.refs || {};
  var elChart = document.getElementById("chart");
  var elEmpty = document.getElementById("empty");
  var btns = Array.prototype.slice.call(document.querySelectorAll(".ranges button"));

  // Картинка товара хотлинкится с CDN маркетплейса и может быть битой/заблокированной
  // по Referer — тогда прячем бокс, чтобы не показывать «сломанный» значок.
  (function () {
    var img = document.getElementById("prodimg");
    if (!img) return;
    function hide() {
      var box = document.getElementById("prodimg-box");
      if (box) box.style.display = "none";
    }
    img.addEventListener("error", hide);
    if (img.complete && img.naturalWidth === 0) hide();
  })();

  if (!elChart || typeof uPlot === "undefined") return;

  var BERRY = "#e8336c", BERRY_HI = "#ff5d8f", MUTED = "#a07f8e",
      GRID = "rgba(232,51,108,.10)", GOOD = "#3fe0a8", GOLD = "#ffc24a";

  var monthFmt = new Intl.DateTimeFormat("ru-RU", { day: "numeric", month: "short" });
  var fullFmt = new Intl.DateTimeFormat("ru-RU", { day: "numeric", month: "long", year: "numeric" });
  var rubFmt0 = new Intl.NumberFormat("ru-RU", { maximumFractionDigits: 0 });
  function rub(v) { return v == null ? "—" : rubFmt0.format(Math.round(v)) + " ₽"; }

  function toCols(points) {
    var xs = [], ys = [];
    for (var i = 0; i < points.length; i++) {
      xs.push(Math.round(points[i][0] / 1000));
      ys.push(points[i][1]);
    }
    return [xs, ys];
  }

  var chart = null;

  // Анимация первой отрисовки: серия «прорастает» слева направо. Оси/сетка не
  // прыгают — на время анимации шкалы зафиксированы по полной серии (fixedX/Y),
  // после — отпускаем автоскейл (те же значения, визуального скачка нет).
  var reduceMotion = window.matchMedia &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var fixedX = null, fixedY = null, animGen = 0;

  function animateIn(full) {
    var xs = full[0], ys = full[1], n = xs.length;
    if (n < 8) return; // короткую серию не мучаем
    var ymin = Infinity, ymax = -Infinity;
    for (var i = 0; i < n; i++) {
      if (ys[i] == null) continue;
      if (ys[i] < ymin) ymin = ys[i];
      if (ys[i] > ymax) ymax = ys[i];
    }
    if (!isFinite(ymin)) return;
    fixedX = [xs[0], xs[n - 1]];
    fixedY = uPlot.rangeNum(ymin, ymax, 0.1, true);
    var gen = ++animGen, dur = 700, t0 = performance.now();
    function frame(t) {
      // Переключение периода (setData в render) обгоняет анимацию — бросаем её.
      if (!chart || gen !== animGen) return;
      var p = Math.min(1, (t - t0) / dur);
      var e = 1 - Math.pow(1 - p, 3); // ease-out cubic
      var k = Math.max(2, Math.round(e * n));
      chart.setData([xs.slice(0, k), ys.slice(0, k)]);
      if (p < 1) requestAnimationFrame(frame);
      else { fixedX = fixedY = null; chart.setData(full); }
    }
    requestAnimationFrame(frame);
  }

  function size() {
    return { width: elChart.clientWidth || 600, height: elChart.clientHeight || 340 };
  }

  // Опорные горизонтальные линии (минимум / обычная) — рисуем поверх области.
  function refLinesPlugin() {
    function draw(u) {
      var lines = [
        { v: refs.min, color: GOOD },
        { v: refs.usual, color: GOLD }
      ];
      var ctx = u.ctx, b = u.bbox;
      ctx.save();
      ctx.lineWidth = 1 * devicePixelRatio;
      ctx.setLineDash([4 * devicePixelRatio, 4 * devicePixelRatio]);
      for (var i = 0; i < lines.length; i++) {
        var ln = lines[i];
        if (!ln.v || ln.v <= 0) continue;
        var y = u.valToPos(ln.v, "y", true);
        if (y < b.top || y > b.top + b.height) continue;
        ctx.strokeStyle = ln.color;
        ctx.globalAlpha = 0.85;
        ctx.beginPath();
        ctx.moveTo(b.left, y);
        ctx.lineTo(b.left + b.width, y);
        ctx.stroke();
      }
      ctx.restore();
    }
    return { hooks: { draw: draw } };
  }

  function tooltipPlugin() {
    var el;
    return {
      hooks: {
        init: function (u) {
          el = document.createElement("div");
          el.className = "u-tip";
          el.style.cssText = "position:absolute;pointer-events:none;z-index:10;display:none;" +
            "background:#210a1a;border:1px solid rgba(232,51,108,.32);border-radius:10px;" +
            "padding:7px 10px;font-size:.8rem;color:#f9f3ef;white-space:nowrap;" +
            "font-feature-settings:'tnum' 1;transform:translate(-50%,-118%);box-shadow:0 6px 22px rgba(0,0,0,.4)";
          u.over.appendChild(el);
        },
        setCursor: function (u) {
          var i = u.cursor.idx;
          if (i == null || u.data[1][i] == null) { el.style.display = "none"; return; }
          var ts = u.data[0][i] * 1000;
          el.innerHTML = "<b>" + rub(u.data[1][i]) + "</b><br><span style='color:#a07f8e'>" +
            fullFmt.format(new Date(ts)) + "</span>";
          el.style.display = "block";
          el.style.left = u.valToPos(u.data[0][i], "x") + "px";
          el.style.top = u.valToPos(u.data[1][i], "y") + "px";
        }
      }
    };
  }

  function render(series) {
    var points = (series && series.points) || [];
    if (points.length < 2) {
      if (chart) { chart.destroy(); chart = null; }
      elChart.hidden = true;
      if (elEmpty) elEmpty.hidden = false;
      return;
    }
    elChart.hidden = false;
    if (elEmpty) elEmpty.hidden = true;

    var data = toCols(points);
    if (chart) {
      animGen++; fixedX = fixedY = null; // оборвать анимацию первой загрузки
      chart.setData(data);
      return;
    }

    var s = size();
    var opts = {
      width: s.width,
      height: s.height,
      padding: [14, 10, 0, 10],
      cursor: { y: false, points: { size: 7, fill: BERRY_HI, stroke: "#150611", width: 2 } },
      legend: { show: false },
      scales: {
        x: { time: true, range: function (u, min, max) { return fixedX || [min, max]; } },
        y: { range: function (u, min, max) { return fixedY || uPlot.rangeNum(min, max, 0.1, true); } }
      },
      plugins: [refLinesPlugin(), tooltipPlugin()],
      axes: [
        {
          stroke: MUTED, grid: { show: false }, ticks: { stroke: GRID, size: 4 },
          font: "12px Onest, sans-serif",
          values: function (u, splits) {
            return splits.map(function (v) { return monthFmt.format(new Date(v * 1000)); });
          }
        },
        {
          stroke: MUTED, grid: { stroke: GRID, width: 1 }, ticks: { show: false },
          font: "12px Onest, sans-serif", size: 62,
          values: function (u, splits) {
            return splits.map(function (v) { return rubFmt0.format(Math.round(v)); });
          }
        }
      ],
      series: [
        {},
        {
          stroke: BERRY, width: 2.2,
          fill: "rgba(232,51,108,.10)",
          paths: uPlot.paths.stepped({ align: 1 }),
          points: { show: false }
        }
      ]
    };
    chart = new uPlot(opts, data, elChart);
    if (!reduceMotion) animateIn(data);
  }

  function setActive(range) {
    btns.forEach(function (b) {
      var on = b.getAttribute("data-range") === range;
      b.classList.toggle("active", on);
      if (on) b.setAttribute("aria-selected", "true"); else b.removeAttribute("aria-selected");
    });
  }

  function load(range) {
    setActive(range);
    fetch("/api/price-history?p=" + encodeURIComponent(publicId) + "&range=" + encodeURIComponent(range),
      { headers: { "Accept": "application/json" } })
      .then(function (r) { if (!r.ok) throw new Error("status " + r.status); return r.json(); })
      .then(function (s) { render(s); })
      .catch(function () { /* оставляем текущий график */ });
  }

  btns.forEach(function (b) {
    b.addEventListener("click", function () { load(b.getAttribute("data-range")); });
  });

  render(boot.series);
  if (boot.series && boot.series.range) setActive(boot.series.range);

  var rt;
  window.addEventListener("resize", function () {
    clearTimeout(rt);
    rt = setTimeout(function () { if (chart) chart.setSize(size()); }, 120);
  });
})();
