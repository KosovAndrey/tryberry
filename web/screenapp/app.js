// Плеер апп-версии (PWA). Проигрывает сценарий чата при появлении экрана —
// т.е. после разблокировки/открытия приложения, синхронно с реальным временем,
// чтобы штамп на сообщениях совпадал с часами iOS. Строку ввода не трогаем (она fixed).
(function () {
  var app = document.getElementById('app');
  var thread = document.getElementById('thread');
  if (!app || !thread) return;
  var timer;
  function pad(n) { return (n < 10 ? '0' : '') + n; }

  // Плавный скролл ленты вниз (следим за новым сообщением).
  function scrollToBottom(dur) {
    var from = thread.scrollTop;
    var to = thread.scrollHeight - thread.clientHeight;
    if (to <= from + 0.5) return;
    var t0 = null;
    function step(ts) {
      if (t0 === null) t0 = ts;
      var p = Math.min(1, (ts - t0) / dur);
      var e = 1 - Math.pow(1 - p, 3);
      thread.scrollTop = from + (to - from) * e;
      if (p < 1) requestAnimationFrame(step);
    }
    requestAnimationFrame(step);
  }

  function hide() {                 // вернуть в «пустое» состояние (до анимаций)
    clearTimeout(timer);
    app.classList.remove('play');
    thread.scrollTop = 0;
  }

  function play() {
    clearTimeout(timer);
    var now = new Date();
    thread.querySelectorAll('.msg time').forEach(function (el) {
      el.textContent = pad(now.getHours()) + ':' + pad(now.getMinutes());
    });
    app.classList.remove('play');
    thread.scrollTop = 0;
    void app.offsetWidth;           // reflow → перезапуск CSS-анимаций
    app.classList.add('play');
    // следуем за появлением сообщений (тайминги = задержки в screen.css)
    setTimeout(function () { scrollToBottom(650); }, 1650);   // после m1
    setTimeout(function () { scrollToBottom(700); }, 3800);   // после m2
    // через ~12 с чат снова «пустой» (готов к повторному дублю записи)
    timer = setTimeout(hide, 12000);
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) hide();    // экран погас / свернули → прячем
    else play();                    // разблокировали / открыли → играем с начала
  });
  window.addEventListener('pagehide', hide);
  window.addEventListener('pageshow', play);
  play();
})();
