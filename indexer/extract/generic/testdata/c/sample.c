#include <stdio.h>
#include "store.h"

#define MAX_ITEMS 10
#define SQUARE(x) ((x) * (x))

/* A stored item. */
struct item {
    int id;
    char *name;
};

typedef struct item item_t;

enum color { RED, GREEN };

static int counter = 0;
int total;

int store_add(struct item *it);

// Adds an item.
int store_add(struct item *it) {
    printf("%d\n", it->id);
    counter++;
    return helper(it->id);
}

static void helper_internal(void) {
    store_add(NULL);
}
