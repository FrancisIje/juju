(() => {
  // cart.js
  function lineTotal(item) {
    return item.price * item.qty;
  }
  function cartTotal(cart) {
    let sum = 0;
    for (const item of cart.items) {
      sum += lineTotal(item);
    }
    return sum;
  }

  // main.js
  function checkout(cart) {
    const total = cartTotal(cart);
    if (total > 100) {
      throw new Error("total too high: " + total);
    }
    return total;
  }
  window.checkout = checkout;
})();
//# sourceMappingURL=multiline.js.map
