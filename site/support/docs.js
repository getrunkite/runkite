/* Docs handbook helpers: copy blocks, keep sidebar scroll, pending badges. */
(function () {
  var SIDEBAR_KEY = "runkite-docs-nav-scroll";

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
      wrap.appendChild(btn);

      btn.addEventListener("click", function (ev) {
        var target = ev.currentTarget;
        var code = target.parentElement.querySelector("pre");
        var text = code ? code.textContent : "";
        function ok() {
          target.textContent = "Copied";
          target.classList.add("is-copied");
          setTimeout(function () {
            target.textContent = "Copy";
            target.classList.remove("is-copied");
          }, 1400);
        }
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(text).then(ok).catch(function () {
            fallbackCopy(text, ok);
          });
        } else {
          fallbackCopy(text, ok);
        }
      });
    }
  }

  function fallbackCopy(text, ok) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.setAttribute("readonly", "");
    ta.style.position = "fixed";
    ta.style.left = "-9999px";
    document.body.appendChild(ta);
    ta.select();
    try {
      document.execCommand("copy");
      ok();
    } catch (_) {}
    document.body.removeChild(ta);
  }

  function keepSidebarScroll() {
    var sidebar = document.querySelector(".sidebar");
    if (!sidebar) return;

    var saved = sessionStorage.getItem(SIDEBAR_KEY);
    if (saved !== null) {
      sidebar.scrollTop = parseInt(saved, 10) || 0;
    }

    var active = sidebar.querySelector("a.active");
    if (active) {
      // Keep the current item in view without yanking the list to the top.
      try {
        active.scrollIntoView({ block: "nearest", inline: "nearest" });
      } catch (_) {
        /* older browsers */
      }
    }

    function persist() {
      sessionStorage.setItem(SIDEBAR_KEY, String(sidebar.scrollTop));
    }
    sidebar.addEventListener("scroll", persist, { passive: true });
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
      td.title = "Known gap, on the roadmap: universal checkpoints for non-LangGraph adapters.";
      td.setAttribute("data-pending", "adapter-checkpoints");
    }
  }

  ready(function () {
    enhanceCodeBlocks();
    keepSidebarScroll();
    markPendingCells();
  });
})();
