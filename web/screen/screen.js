// Общий сценарий-плеер для страниц записи /screen1../screenN.
// Время сообщений = реальное на момент показа (совпадает со статус-баром iOS).
// Перезапуск анимаций при появлении вкладки (ровно когда после разблокировки
// Safari выходит на передний план).
(function () {
  var stage = document.getElementById('stage');
  if (!stage) return;
  function pad(n) { return (n < 10 ? '0' : '') + n; }
  function play() {
    var now = new Date();
    stage.querySelectorAll('.msg time').forEach(function (el) {
      var off = parseInt(el.getAttribute('data-min') || '0', 10); // сдвиг в минутах (опц.)
      var d = new Date(now.getTime() + off * 60000);
      el.textContent = pad(d.getHours()) + ':' + pad(d.getMinutes());
    });
    stage.classList.remove('play');
    void stage.offsetWidth; // reflow → перезапуск CSS-анимаций
    stage.classList.add('play');
  }
  document.addEventListener('visibilitychange', function () { if (!document.hidden) play(); });
  window.addEventListener('pageshow', play);
  play();
})();
