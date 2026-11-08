/* Docs handbook helpers: copy blocks, keep sidebar scroll, pending badges. */
(function () {
  var SIDEBAR_KEY = "runkite-docs-nav-scroll";
  var NAV_HORIZONTAL_KEY = SIDEBAR_KEY + "-x";

  function ready(fn) {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", fn);
    } else {
      fn();
    }
  }

  function enhanceCodeBlocks() {
    var blocks = document.querySelectorAll("pre.mono-block");
    for (var i = 0; i < blocks.length; i++) {
      var pre = blocks[i];
      if (pre.parentElement && pre.parentElement.classList.contains("code-block")) continue;
      var wrap = document.createElement("div");
      wrap.className = "code-block";
      pre.parentNode.insertBefore(wrap, pre);
      wrap.appendChild(pre);

      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy-btn";
      btn.textContent = "Copy";
      btn.setAttribute("aria-label", "Copy code");
      btn.setAttribute("aria-live", "polite");
      wrap.appendChild(btn);

      btn.addEventListener("click", function (ev) {
        var target = ev.currentTarget;
        if (target.copyPending) return;
        var code = target.parentElement.querySelector("pre");
        var text = code ? code.textContent : "";
        clearTimeout(target.copyReset);
        target.copyPending = true;
        target.setAttribute("aria-busy", "true");
        target.textContent = "Copying";
        target.setAttribute("aria-label", "Copying code");
        target.classList.remove("is-copied", "is-error");
        target.removeAttribute("title");
        function ok() {
          target.copyPending = false;
          target.removeAttribute("aria-busy");
          target.textContent = "Copied";
          target.setAttribute("aria-label", "Copied code");
          target.classList.add("is-copied");
          target.copyReset = setTimeout(function () {
            target.textContent = "Copy";
            target.setAttribute("aria-label", "Copy code");
            target.classList.remove("is-copied");
          }, 1400);
        }
        function failed() {
          target.copyPending = false;
          target.removeAttribute("aria-busy");
          target.textContent = "Copy failed";
          target.setAttribute("aria-label", "Copy failed. Select and copy the code manually.");
          target.setAttribute("title", "Select and copy the code manually, or try again.");
          target.classList.add("is-error");
        }
        try {
          if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard.writeText(text).then(ok).catch(function () {
              fallbackCopy(text, ok, failed);
            });
          } else {
            fallbackCopy(text, ok, failed);
          }
        } catch (_) {
          fallbackCopy(text, ok, failed);
        }
      });
    }
  }

  function fallbackCopy(text, ok, failed) {
    var previousFocus = document.activeElement;
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    ta.style.top = "0";
    document.body.appendChild(ta);
    var copied = false;
    try {
      ta.focus({ preventScroll: true });
      ta.select();
      copied = document.execCommand("copy") === true;
    } catch (_) {}
    document.body.removeChild(ta);
    if (previousFocus && previousFocus.focus) previousFocus.focus({ preventScroll: true });
    if (copied) ok();
    else failed();
  }

  function savedOffset(key) {
    try {
      var value = parseFloat(sessionStorage.getItem(key));
      return isFinite(value) ? Math.max(0, value) : 0;
    } catch (_) {
      return 0;
    }
  }

  function saveOffset(key, value) {
    try {
      sessionStorage.setItem(key, String(value));
    } catch (_) {
      // Navigation and other helpers do not depend on browser storage.
    }
  }

  function keepSidebarScroll() {
    var sidebar = document.querySelector(".sidebar");
    if (!sidebar) return;

    var nav = sidebar.querySelector("nav");
    sidebar.scrollTop = savedOffset(SIDEBAR_KEY);
    if (nav) nav.scrollLeft = savedOffset(NAV_HORIZONTAL_KEY);
    var active = sidebar.querySelector("a.active");

    function revealActive() {
      if (!active) return;
      // Scroll only the navigation containers. Scrolling the link's ancestors
      // would also move the document away from a requested section anchor.
      var box = active.getBoundingClientRect();
      if (sidebar.scrollHeight > sidebar.clientHeight) {
        var sideBox = sidebar.getBoundingClientRect();
        if (box.top < sideBox.top) sidebar.scrollTop += box.top - sideBox.top;
        else if (box.bottom > sideBox.bottom) sidebar.scrollTop += box.bottom - sideBox.bottom;
      }
      if (nav && nav.scrollWidth > nav.clientWidth) {
        var navBox = nav.getBoundingClientRect();
        var style = window.getComputedStyle(nav);
        var left = navBox.left + (parseFloat(style.scrollPaddingLeft) || 0);
        var right = navBox.right - (parseFloat(style.scrollPaddingRight) || 0);
        if (box.left < left) nav.scrollLeft += box.left - left;
        else if (box.right > right) nav.scrollLeft += box.right - right;
      }
    }
    revealActive();
    window.addEventListener("resize", revealActive);

    function persist() {
      saveOffset(SIDEBAR_KEY, sidebar.scrollTop);
      if (nav) saveOffset(NAV_HORIZONTAL_KEY, nav.scrollLeft);
    }
    sidebar.addEventListener("scroll", persist, { passive: true });
    if (nav) nav.addEventListener("scroll", persist, { passive: true });
    var links = sidebar.querySelectorAll("a[href]");
    for (var i = 0; i < links.length; i++) {
      links[i].addEventListener("click", persist);
    }
  }

  function markPendingCells() {
    var cells = document.querySelectorAll("table.matrix td");
    for (var i = 0; i < cells.length; i++) {
      var td = cells[i];
      if (td.textContent.trim() !== "Not yet") continue;
      td.classList.add("pending");
      td.title = "Known gap, not yet implemented for this framework.";
      td.setAttribute("data-pending", "matrix-gap");
    }
  }

  function adminHref() {
    if (location.hostname === "getrunkite.github.io") {
      return "https://getrunkite.com/admin/";
    }
    return "/admin/";
  }

  function markDemoOffline(link) {
    link.setAttribute("data-demo-offline", "");
    link.setAttribute("title", "Shared live demo currently unavailable; local self-hosting is available.");
    link.setAttribute("aria-label", link.textContent + " (shared live demo currently unavailable; local self-hosting is available)");
  }

  function injectLiveAdmin() {
    var href = adminHref();
    if (!document.getElementById("rk-live-admin-style")) {
      var style = document.createElement("style");
      style.id = "rk-live-admin-style";
      style.textContent =
        ".sidebar nav a.nav-live-admin{color:var(--accent);font-weight:600;box-shadow:inset 3px 0 0 var(--accent);margin-bottom:.85rem}" +
        ".topbar a[data-live-admin]{color:var(--accent-2);font-weight:600}";
      document.head.appendChild(style);
    }

    var topbar = document.querySelector(".topbar");
    if (topbar && !topbar.querySelector("[data-live-admin]")) {
      var top = document.createElement("a");
      top.href = href;
      top.setAttribute("data-live-admin", "1");
      top.textContent = "Live Admin";
      markDemoOffline(top);
      var product = null;
      var links = topbar.querySelectorAll("a[href]");
      for (var i = 0; i < links.length; i++) {
        var h = links[i].getAttribute("href") || "";
        if (h === "../" || h === "../../" || h === "../index.html") {
          product = links[i];
          break;
        }
      }
      if (product && product.nextSibling) {
        topbar.insertBefore(top, product.nextSibling);
      } else {
        topbar.insertBefore(top, topbar.firstChild);
      }
    }

    var nav = document.querySelector(".sidebar nav");
    if (nav && !nav.querySelector("[data-live-admin]")) {
      var side = document.createElement("a");
      side.href = href;
      side.setAttribute("data-live-admin", "1");
      side.className = "nav-live-admin";
      side.textContent = "Live Admin";
      markDemoOffline(side);
      nav.insertBefore(side, nav.firstChild);
    }
  }

  ready(function () {
    enhanceCodeBlocks();
    markPendingCells();
    injectLiveAdmin();
    keepSidebarScroll();
  });
})();
