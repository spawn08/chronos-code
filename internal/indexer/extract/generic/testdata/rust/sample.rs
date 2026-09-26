//! Crate docs.
use std::collections::HashMap;
use crate::store::{Repo, Cache as LruCache};
use super::util::*;
pub use self::model::User;
pub use crate::errors::Error as StoreError;

mod model;

/// A key-value store.
#[derive(Debug, Clone)]
pub struct Store<K> {
    pub name: String,
    items: HashMap<K, String>,
}

pub(crate) enum Mode {
    Read,
    Write(u32),
}

pub trait Backend: Send {
    fn get(&self, key: &str) -> Option<String>;
    fn put(&mut self, key: &str) {}
}

impl<K> Store<K> {
    /// Creates a store.
    pub fn new(name: &str) -> Self {
        let items = HashMap::new();
        helper(1);
        Store { name: name.to_string(), items }
    }

    async fn flush(&self) {
        self.items.clear();
    }
}

impl<K> Backend for Store<K> {
    fn get(&self, key: &str) -> Option<String> {
        util::lookup(key)
    }
}

pub const LIMIT: usize = 10;
static COUNTER: u32 = 0;
type Result<T> = std::result::Result<T, StoreError>;

macro_rules! log {
    ($x:expr) => { println!("{}", $x) };
}

#[deprecated]
fn helper(n: i32) -> i32 {
    n + 1
}

#[cfg(test)]
mod tests {
    #[test]
    fn it_works() {
        super::helper(2);
    }
}
