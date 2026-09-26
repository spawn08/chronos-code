<?php

namespace App\Store;

use App\Base\BaseRepo;
use App\Contracts\{Repo, Cache as LruCache};
use function App\Util\helper;

/**
 * A user repository.
 */
#[Service]
final class UserRepo extends BaseRepo implements Repo, \Countable
{
    use Logging;

    public const LIMIT = 10;
    private array $users = [];
    protected static int $count = 0;

    public function __construct(private Db $db)
    {
        parent::__construct();
    }

    public function find(string $id): ?User
    {
        Helper::check($id);
        $u = new User($id);
        $this->log("find");
        return helper($id);
    }

    /** @deprecated */
    abstract protected function legacy(): void;
}

interface Repo
{
    public function find(string $id): ?User;
}

trait Logging
{
    public function log(string $m): void {}
}

enum Mode: string
{
    case Read = 'r';
    case Write = 'w';
}

function top_level(int $x): int
{
    return helper($x);
}
