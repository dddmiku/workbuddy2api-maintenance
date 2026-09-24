// ═══ 更新日志 ═══
// 2026-09-24：浮动表头对齐内容滚动区与顶栏下沿，避免固定顶栏遮住表头，并在原表末尾收起。
// 2026-09-19：固定长表格表头，跟随原表格列宽和水平位置；保留页面自然纵向滚动。
(function(){
  function initTableHeaders(){
    var pending = false;
    var tables = [];
    function schedule(){
      if (pending) return;
      pending = true;
      requestAnimationFrame(function(){
        pending = false;
        tables.forEach(sync);
      });
    }
    function sync(state){
      var table = state.table, wrap = state.wrap, floating = state.floating;
      if (!table.getClientRects().length){ floating.hidden = true; return; }
      var head = table.tHead, headBox = head.getBoundingClientRect();
      var tableBox = table.getBoundingClientRect(), wrapBox = wrap.getBoundingClientRect();
      var stickyTop = state.scroller ? Math.max(0, state.scroller.getBoundingClientRect().top + state.scroller.clientTop) : 0;
      if (state.topbar && state.topbar.getClientRects().length){
        stickyTop = Math.max(stickyTop, state.topbar.getBoundingClientRect().bottom);
      }
      if (headBox.top >= stickyTop || tableBox.bottom <= stickyTop || !wrap.clientWidth){
        floating.hidden = true;
        return;
      }
      // The original overflow-x wrapper traps CSS sticky positioning. A visual
      // copy outside it follows the source; the original header stays accessible.
      if (state.markup !== head.innerHTML){
        state.markup = head.innerHTML;
        state.clone.replaceChildren(head.cloneNode(true));
        state.clone.querySelectorAll('[id]').forEach(function(node){ node.removeAttribute('id'); });
      }
      var sourceCells = head.rows[0].cells;
      var copiedCells = state.clone.tHead.rows[0].cells;
      Array.prototype.forEach.call(sourceCells, function(cell, index){
        copiedCells[index].style.width = cell.getBoundingClientRect().width + 'px';
      });
      state.clone.style.width = tableBox.width + 'px';
      state.clone.style.transform = 'translateX(' + (-wrap.scrollLeft) + 'px)';
      floating.style.left = wrapBox.left + 'px';
      floating.style.width = wrap.clientWidth + 'px';
      floating.style.height = headBox.height + 'px';
      floating.style.top = Math.min(stickyTop, tableBox.bottom - headBox.height) + 'px';
      floating.hidden = false;
    }
    document.querySelectorAll('table[data-sticky-head]').forEach(function(table){
      var wrap = table.closest('.tblwrap');
      var scroller = table.closest('.body'), topbar = document.querySelector('.top');
      var floating = document.createElement('div');
      floating.className = 'table-head-float';
      floating.dataset.tableHeadFor = table.dataset.stickyHead;
      floating.hidden = true;
      floating.setAttribute('aria-hidden', 'true');
      floating.setAttribute('inert', '');
      var clone = document.createElement('table');
      clone.className = table.className;
      floating.appendChild(clone);
      document.body.appendChild(floating);
      tables.push({table:table, wrap:wrap, scroller:scroller, topbar:topbar, floating:floating, clone:clone, markup:null});
      new ResizeObserver(schedule).observe(table);
      new ResizeObserver(schedule).observe(wrap);
      if (scroller) new ResizeObserver(schedule).observe(scroller);
      if (topbar) new ResizeObserver(schedule).observe(topbar);
      new MutationObserver(schedule).observe(table, {childList:true, subtree:true, characterData:true});
      new MutationObserver(schedule).observe(table.closest('.view'), {attributes:true, attributeFilter:['class']});
    });
    window.addEventListener('scroll', schedule, {capture:true, passive:true});
    window.addEventListener('resize', schedule, {passive:true});
    if (document.fonts) document.fonts.ready.then(schedule);
    schedule();
  }
  if (document.readyState === 'loading'){
    document.addEventListener('DOMContentLoaded', initTableHeaders);
  } else {
    initTableHeaders();
  }
})();
