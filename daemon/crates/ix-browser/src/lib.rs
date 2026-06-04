pub mod backend;
pub mod noop;
pub mod pinchtab;
pub mod remote;
pub mod wait;

pub use backend::BrowserBackend;
pub use noop::NoopBrowserBackend;
pub use pinchtab::PinchtabBackend;
pub use remote::RemoteSharedBrowserBackend;
