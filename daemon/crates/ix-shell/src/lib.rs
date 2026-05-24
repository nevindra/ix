pub mod exec;
pub mod signal;

pub use exec::execute_shell;

#[cfg(test)]
mod tests;
