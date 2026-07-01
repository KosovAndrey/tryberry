// Общий сценарий-плеер для страниц записи /screenN.
// Время сообщений = реальное на момент показа (совпадает со статус-баром iOS).
// Проигрывается при появлении вкладки (когда после разблокировки Safari выходит
// на передний план). Чтобы под «шторкой» разблокировки не мелькал старый кадр:
//  • сразу прячем всё, когда страница уходит в фон (экран гаснет);
//  • и авто-прячем через 10 с после показа.
(function () {
  var stage = document.getElementById('stage');
  if (!stage) return;
  var timer;
  function pad(n) { return (n < 10 ? '0' : '') + n; }

  function hide() {                 // вернуть в пустое состояние (до анимаций)
    clearTimeout(timer);
    stage.classList.remove('play');
  }

  function play() {
    clearTimeout(timer);
    var now = new Date();
    stage.querySelectorAll('.msg time').forEach(function (el) {
      var off = parseInt(el.getAttribute('data-min') || '0', 10);
      var d = new Date(now.getTime() + off * 60000);
      el.textContent = pad(d.getHours()) + ':' + pad(d.getMinutes());
    });
    stage.classList.remove('play');
    void stage.offsetWidth;         // reflow → перезапуск CSS-анимаций
    stage.classList.add('play');
    // спрятать всё через ~10 с после показа (анимация ~5 с) → чат снова «пустой»
    timer = setTimeout(hide, 15000);
  }

  document.addEventListener('visibilitychange', function () {
    if (document.hidden) hide();    // экран погас / свернули → прячем немедленно
    else play();                    // разблокировали / вернулись → играем с начала
  });
  window.addEventListener('pagehide', hide);
  window.addEventListener('pageshow', play);
  play();
})();
