// MyVoIP の Web 管理画面（CSP によりインラインのスクリプトは使えないため、このファイルにまとめる）
(function () {
  "use strict";

  // 削除などの前に確認する
  document.addEventListener("submit", function (event) {
    var form = event.target;
    var message = form.getAttribute("data-confirm");
    if (message && !window.confirm(message)) {
      event.preventDefault();
    }
  });

  // 登録用リンクをコピーする。http では navigator.clipboard が使えないため、選択してコピーする方法も使う
  document.addEventListener("click", function (event) {
    var button = event.target.closest("[data-copy]");
    if (!button) {
      return;
    }
    var source = button.parentElement.querySelector(".copy-source");
    if (!source) {
      return;
    }
    var label = button.getAttribute("data-label") || button.textContent;
    button.setAttribute("data-label", label);
    var show = function (text) {
      button.textContent = text;
      setTimeout(function () { button.textContent = label; }, 2500);
    };
    var done = function () { show("コピーしました"); };
    var fallback = function () {
      source.focus();
      source.select();
      source.setSelectionRange(0, source.value.length);
      var copied = false;
      try {
        copied = document.execCommand("copy");
      } catch (e) {
        copied = false;
      }
      // コピーできなければ選択した状態のまま残し、利用者が手でコピーできるようにする
      show(copied ? "コピーしました" : "選択しました。長押しでコピー");
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(source.value).then(done, fallback);
    } else {
      fallback();
    }
  });

  // 読み取り専用の入力欄はタップで全体を選択する
  document.addEventListener("focusin", function (event) {
    if (event.target.classList && event.target.classList.contains("copy-source")) {
      event.target.select();
    }
  });
})();
